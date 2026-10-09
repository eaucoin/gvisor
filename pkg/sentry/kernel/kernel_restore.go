// Copyright 2024 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kernel

import (
	"fmt"
	"io"
	"math"

	"gvisor.dev/gvisor/pkg/cleanup"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	pgallocpb "gvisor.dev/gvisor/pkg/sentry/pgalloc/pgalloc_metadata_go_proto"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/timing"
)

// Saver is an interface for saving the kernel.
type Saver interface {
	SaveAsync() error
	SpecEnviron(containerName string) []string
	FSSave() error
}

// CheckpointGeneration stores information about the last checkpoint taken.
//
// +stateify savable
type CheckpointGeneration struct {
	// Count is incremented every time a checkpoint is triggered, even if the
	// checkpoint failed.
	Count uint32
	// Restore indicates if the current instance resumed after the checkpoint or
	// it was restored from a checkpoint.
	Restore bool
}

// AddStateToCheckpoint adds a key-value pair to be additionally checkpointed.
func (k *Kernel) AddStateToCheckpoint(key, v any) {
	k.checkpointMu.Lock()
	defer k.checkpointMu.Unlock()
	if k.additionalCheckpointState == nil {
		k.additionalCheckpointState = make(map[any]any)
	}
	k.additionalCheckpointState[key] = v
}

// PopCheckpointState pops a key-value pair from the additional checkpoint
// state. If the key doesn't exist, nil is returned.
func (k *Kernel) PopCheckpointState(key any) any {
	k.checkpointMu.Lock()
	defer k.checkpointMu.Unlock()
	if v, ok := k.additionalCheckpointState[key]; ok {
		delete(k.additionalCheckpointState, key)
		return v
	}
	return nil
}

// SetSaver sets the kernel's Saver.
// Thread-compatible.
func (k *Kernel) SetSaver(s Saver) {
	k.checkpointMu.Lock()
	defer k.checkpointMu.Unlock()
	k.saver = s
}

// Saver returns the kernel's Saver.
// Thread-compatible.
func (k *Kernel) Saver() Saver {
	k.checkpointMu.Lock()
	defer k.checkpointMu.Unlock()
	return k.saver
}

// CheckpointGen returns the current checkpoint generation.
func (k *Kernel) CheckpointGen() CheckpointGeneration {
	k.checkpointMu.Lock()
	defer k.checkpointMu.Unlock()

	return k.checkpointGen
}

// IncCheckpointGenOnRestore increments the checkpoint generation upon restore.
func (k *Kernel) IncCheckpointGenOnRestore() {
	k.checkpointMu.Lock()
	defer k.checkpointMu.Unlock()

	k.checkpointGen.Count++
	k.checkpointGen.Restore = true

	k.CheckpointWait.signal(k.checkpointGen, nil)
}

// OnCheckpointAttempt is called when a checkpoint attempt is completed. err is
// any checkpoint errors that may have occurred.
func (k *Kernel) OnCheckpointAttempt(err error) {
	if err == nil {
		log.Infof("Checkpoint completed successfully.")
	} else {
		log.Warningf("Checkpoint attempt failed with error: %v", err)
	}

	k.checkpointMu.Lock()
	defer k.checkpointMu.Unlock()

	k.checkpointGen.Count++
	k.checkpointGen.Restore = false

	k.CheckpointWait.signal(k.checkpointGen, err)
}

// WaitForCheckpoint waits for the Kernel to have been successfully checkpointed.
func (k *Kernel) WaitForCheckpoint() error {
	// Send checkpoint result to a channel and wait on it.
	ch := make(chan error, 1)
	callback := func(_ CheckpointGeneration, err error) { ch <- err }
	key := k.CheckpointWait.Register(callback, k.CheckpointGen().Count+1)
	defer k.CheckpointWait.Unregister(key)

	return <-ch
}

// SignalAllCheckpointWaiters signals all checkpoint waiters with err.
func (k *Kernel) SignalAllCheckpointWaiters(err error) {
	k.CheckpointWait.signal(CheckpointGeneration{Count: math.MaxUint32}, err)
}

type checkpointWaiter struct {
	// count indicates the checkpoint generation that this waiter is interested in.
	count uint32
	// callback is the function that will be called when the checkpoint generation
	// reaches the desired count. It is set to nil after the callback is called.
	callback func(CheckpointGeneration, error)
}

// CheckpointWaitable is a waitable object that waits for a
// checkpoint to complete.
//
// +stateify savable
type CheckpointWaitable struct {
	k *Kernel

	mu sync.Mutex `state:"nosave"`

	// Don't save the waiters, because they are repopulated after restore. It also
	// allows for external entities to wait for the checkpoint.
	waiters map[*checkpointWaiter]struct{} `state:"nosave"`
}

// Register registers a callback that is notified when the checkpoint generation count is higher
// than the desired count.
func (w *CheckpointWaitable) Register(cb func(CheckpointGeneration, error), count uint32) any {
	w.mu.Lock()
	defer w.mu.Unlock()

	waiter := &checkpointWaiter{
		count:    count,
		callback: cb,
	}
	if w.waiters == nil {
		w.waiters = make(map[*checkpointWaiter]struct{})
	}
	w.waiters[waiter] = struct{}{}

	if gen := w.k.CheckpointGen(); count <= gen.Count {
		// The checkpoint has already occurred. Signal immediately.
		waiter.callback(gen, nil)
		waiter.callback = nil
	}
	return waiter
}

// Unregister unregisters a waiter. It must be called even if the channel
// was signalled.
func (w *CheckpointWaitable) Unregister(key any) {
	w.mu.Lock()
	defer w.mu.Unlock()

	delete(w.waiters, key.(*checkpointWaiter))
	if len(w.waiters) == 0 {
		w.waiters = nil
	}
}

func (w *CheckpointWaitable) signal(gen CheckpointGeneration, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for waiter := range w.waiters {
		if waiter.callback != nil && waiter.count <= gen.Count {
			waiter.callback(gen, err)
			waiter.callback = nil
		}
	}
}

// privateMemoryFileOwners returns the owners of the private MemoryFiles of
// the image whose image-level metadata is image, in the order in which their
// metadata is saved.
func privateMemoryFileOwners(image *pgallocpb.ImageProto) []checkpoint.ResourceID {
	owners := make([]checkpoint.ResourceID, len(image.GetPrivateMemoryFiles()))
	for i, id := range image.GetPrivateMemoryFiles() {
		owners[i] = checkpoint.ResourceID{
			ContainerName: id.GetContainerName(),
			Path:          id.GetPath(),
		}
	}
	return owners
}

// loadPrivateMemoryFiles loads the private MemoryFiles from mfmap and it reads
// private MemoryFile metadata from `r`, in the order of the owners recorded in
// opts.Image. This consumes bytes from `r`, so this must be called only once.
func loadPrivateMemoryFiles(ctx context.Context, r io.Reader, mfmap map[checkpoint.ResourceID]*pgalloc.MemoryFile, fsCheckpointed map[checkpoint.ResourceID]struct{}, opts *pgalloc.LoadOpts) error {
	owners := privateMemoryFileOwners(opts.Image)
	// Ensure that it is consistent with mfmap, unless we are restoring from
	// split filesystem checkpoint.
	if fsCheckpointed != nil {
		ownersMap := make(map[checkpoint.ResourceID]struct{}, len(owners))
		for _, fsID := range owners {
			ownersMap[fsID] = struct{}{}
		}

		// Check that all expected are loaded.
		for fsID := range mfmap {
			_, inFS := fsCheckpointed[fsID]
			_, inSentry := ownersMap[fsID]
			if !inFS && !inSentry {
				return fmt.Errorf("private memory file %q was neither in FS checkpoint nor in Sentry checkpoint", fsID)
			}
			if inFS && inSentry {
				return fmt.Errorf("private memory file %q was present in both FS checkpoint and Sentry checkpoint", fsID)
			}
		}

		// Load from sentry checkpoint.
		for _, fsID := range owners {
			mf, ok := mfmap[fsID]
			if !ok {
				return fmt.Errorf("saved private memory file %q was not configured on restore", fsID)
			}
			err := mf.LoadFrom(ctx, r, opts)
			if err != nil {
				return fmt.Errorf("failed to load MemoryFile %p fsID %q from Sentry state: %w", mf, fsID, err)
			}
		}
		return nil
	}
	if len(mfmap) != len(owners) {
		return fmt.Errorf("inconsistent private memory files on restore: savedMFOwners = %v, mfmap = %v", owners, mfmap)
	}
	// Load all private memory files.
	for _, fsID := range owners {
		mf, ok := mfmap[fsID]
		if !ok {
			return fmt.Errorf("saved private memory file %q was not configured on restore", fsID)
		}
		err := mf.LoadFrom(ctx, r, opts)
		if err != nil {
			return fmt.Errorf("failed to load MemoryFile %p fsID %q: %w", mf, fsID, err)
		}
	}
	return nil
}

// loadMemoryFiles loads MemoryFiles saved inline in the state file r.
func (k *Kernel) loadMemoryFiles(ctx context.Context, r io.Reader) error {
	opts := pgalloc.LoadOpts{Image: &pgallocpb.ImageProto{}}
	if err := checkpointimage.ReadRecord(r, opts.Image, checkpointimage.MaxBodySize); err != nil {
		return fmt.Errorf("failed to read image metadata: %w", err)
	}
	if err := checkpointimage.ValidateImage(opts.Image); err != nil {
		return err
	}
	if err := k.mf.LoadFrom(ctx, r, &opts); err != nil {
		return fmt.Errorf("failed to load main MemoryFile %p: %w", k.mf, err)
	}
	if err := loadPrivateMemoryFiles(ctx, r, pgalloc.MemoryFileMapFromContext(ctx), FSCheckpointedMemoryFilesFromContext(ctx), &opts); err != nil {
		return fmt.Errorf("failed to load private MemoryFiles: %w", err)
	}
	return nil
}

// AsyncMFLoader loads all MemoryFiles asynchronously; thanks to having
// separate pages and page metadata files, as opposed to a single state file
// containing everything.
//
// The AsyncMFLoader helps achieve the following goals:
//   - Loading the main MemoryFile as early as possible.
//   - Loading the private MemoryFiles, also as early as possible but after
//     the main MemoryFile.
//   - Wait for various events to occur before proceeding.
//   - Report errors as they occur.
type AsyncMFLoader struct {
	// image is the image being loaded. image is immutable.
	image *checkpointimage.Image

	// privateMFsChan is used to tell the background goroutine about private
	// MemoryFiles, once they are known. This channel is written to exactly once.
	privateMFsChan chan privateMFsInfo

	mainMFStartWg   sync.WaitGroup
	mainMetadataErr error

	metadataWg  sync.WaitGroup
	metadataErr error

	loadWg  sync.WaitGroup
	loadMu  sync.Mutex
	loadErr error
}

type privateMFsInfo struct {
	mfmap          map[checkpoint.ResourceID]*pgalloc.MemoryFile
	fsCheckpointed map[checkpoint.ResourceID]struct{}
}

// NewAsyncMFLoader creates a new AsyncMFLoader of the MemoryFiles of image,
// whose layers' pages files are pagesFiles. It takes ownership of pagesFiles.
// It creates a background goroutine that will load all the MemoryFiles. The
// background goroutine immediately starts loading the main MemoryFile.
// If timeline is provided, it will be used to track async page loading.
// It takes ownership of the timeline, and will end it when done loading all
// pages.
func NewAsyncMFLoader(image *checkpointimage.Image, pagesFiles []stateio.AsyncReader, mainMF *pgalloc.MemoryFile, timeline *timing.Timeline) *AsyncMFLoader {
	mfl := &AsyncMFLoader{
		image:          image,
		privateMFsChan: make(chan privateMFsInfo, 1),
	}
	mfl.mainMFStartWg.Add(1)
	mfl.metadataWg.Add(1)
	mfl.loadWg.Add(1)
	go mfl.backgroundGoroutine(image, pagesFiles, mainMF, timeline)
	return mfl
}

func (mfl *AsyncMFLoader) backgroundGoroutine(image *checkpointimage.Image, pagesFiles []stateio.AsyncReader, mainMF *pgalloc.MemoryFile, timeline *timing.Timeline) {
	defer timeline.End()
	cu := cleanup.Make(func() {
		mfl.metadataWg.Done()
		mfl.loadWg.Done()
	})
	defer cu.Clean()

	// Start loading from the pages file of every layer.
	opts := pgalloc.LoadOpts{
		Image:      image.Proto,
		PagesFiles: make([]*pgalloc.AsyncPagesFileLoad, len(pagesFiles)),
		Timeline:   timeline,
	}
	for i, pagesFile := range pagesFiles {
		mfl.loadWg.Add(1)
		apfl, err := pgalloc.StartAsyncPagesFileLoad(pagesFile, func(err error) {
			defer mfl.loadWg.Done()
			mfl.setLoadErr(err)
		}, timeline) // transfers ownership of pagesFile
		if err != nil {
			mfl.loadWg.Done()
			for _, pagesFile := range pagesFiles[i+1:] {
				pagesFile.Close()
			}
			err = fmt.Errorf("failed to start async page loading from layer %d: %w", i, err)
			log.Warningf("%v", err)
			mfl.mainMetadataErr = err
			mfl.metadataErr = err
			mfl.mainMFStartWg.Done()
			return
		}
		cu.Add(apfl.MemoryFilesDone)
		opts.PagesFiles[i] = apfl
	}

	timeline.Reached("loading mainMF")
	log.Infof("Loading metadata for main MemoryFile: %p", mainMF)
	ctx := context.Background()
	records := image.MemoryFileRecords()
	err := mainMF.LoadFrom(ctx, records, &opts)
	mfl.metadataErr = err
	mfl.mainMetadataErr = err
	mfl.mainMFStartWg.Done()
	if err != nil {
		log.Warningf("Failed to load main MemoryFile %p: %v", mainMF, err)
		return
	}
	timeline.Reached("waiting for privateMF info")
	info := <-mfl.privateMFsChan
	timeline.Reached("received privateMFs info")
	log.Infof("Loading metadata for %d private MemoryFiles", len(info.mfmap))
	if err := loadPrivateMemoryFiles(ctx, records, info.mfmap, info.fsCheckpointed, &opts); err != nil {
		log.Warningf("Failed to load private MemoryFiles: %v", err)
		mfl.metadataErr = err
		return
	}

	// Report metadata load completion.
	timeline.Reached("metadata load done")
	log.Infof("All MemoryFile metadata has been loaded")
	cu.Release()()

	// Wait for page loads to complete and report errors.
	mfl.loadWg.Wait()
	if mfl.loadErr != nil {
		timeline.Invalidate("page load failed")
		log.Warningf("Failed to load MemoryFile pages: %v", mfl.loadErr)
		return
	}
	log.Infof("All MemoryFile pages have been loaded.")
}

// setLoadErr records err, if it is the first error of a pages file.
func (mfl *AsyncMFLoader) setLoadErr(err error) {
	if err == nil {
		return
	}
	mfl.loadMu.Lock()
	defer mfl.loadMu.Unlock()
	if mfl.loadErr == nil {
		mfl.loadErr = err
	}
}

// Image returns the image being loaded.
func (mfl *AsyncMFLoader) Image() *checkpointimage.Image {
	return mfl.image
}

// KickoffPrivate notifies the background goroutine of the private MemoryFiles.
func (mfl *AsyncMFLoader) KickoffPrivate(ctx context.Context, mfmap map[checkpoint.ResourceID]*pgalloc.MemoryFile) {
	mfl.privateMFsChan <- privateMFsInfo{
		mfmap:          mfmap,
		fsCheckpointed: FSCheckpointedMemoryFilesFromContext(ctx),
	}
}

// WaitMainMFStart waits for the background goroutine to successfully start
// asynchronously loading the main MemoryFile.
func (mfl *AsyncMFLoader) WaitMainMFStart() error {
	mfl.mainMFStartWg.Wait()
	return mfl.mainMetadataErr
}

// WaitMetadata waits for the background goroutine to successfully complete
// reading all MemoryFile metadata and report any errors.
func (mfl *AsyncMFLoader) WaitMetadata() error {
	mfl.metadataWg.Wait()
	return mfl.metadataErr
}

// Wait waits for the background goroutine to successfully complete fully
// loading all the MemoryFiles and report any errors.
func (mfl *AsyncMFLoader) Wait() error {
	if err := mfl.WaitMetadata(); err != nil {
		return err
	}
	mfl.loadWg.Wait()
	return mfl.loadErr
}
