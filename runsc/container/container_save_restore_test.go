// Copyright 2026 The gVisor Authors.
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

// This file holds the save/restore (checkpoint/restore) tests for a single
// container.

package container

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cenkalti/backoff"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/cleanup"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/sentry/control/controlapi"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/erofs"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/hostmm/hostmmtest"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/state/statefile"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
	"gvisor.dev/gvisor/runsc/checkpointgofer/s3/s3test"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/sandbox"
	"gvisor.dev/gvisor/runsc/specutils"
)

// TestSignalUnkillablePolicyRestore verifies that the SignalUnkillablePolicy
// is preserved across checkpoint and restore.
func TestSignalUnkillablePolicyRestore(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			t.Run("LinuxPolicyPreservedAfterRestore", func(t *testing.T) {
				testConf := *conf
				testConf.SignalUnkillablePolicy = config.SignalUnkillableLinux

				spec, _ := sleepSpecConf(t)
				_, bundleDir, cleanup, err := testutil.SetupContainer(spec, &testConf)
				if err != nil {
					t.Fatalf("error setting up container: %v", err)
				}
				defer cleanup()

				args := Args{
					ID:        testutil.RandomContainerID(),
					Spec:      spec,
					BundleDir: bundleDir,
				}
				cont, err := New(&testConf, args)
				if err != nil {
					t.Fatalf("error creating container: %v", err)
				}
				defer cont.Destroy()
				if err := cont.Start(&testConf); err != nil {
					t.Fatalf("error starting container: %v", err)
				}

				if err := waitForProcessCount(cont, 1); err != nil {
					t.Fatalf("timed out waiting for init process: %v", err)
				}

				// Checkpoint running container.
				dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-restore-unkillable")
				if err != nil {
					t.Fatalf("os.MkdirTemp failed: %v", err)
				}
				defer os.RemoveAll(dir)
				if err := os.Chmod(dir, 0777); err != nil {
					t.Fatalf("error chmoding dir: %v", err)
				}

				if err := cont.Checkpoint(&testConf, dir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelFlateBestSpeed}); err != nil {
					t.Fatalf("error checkpointing container: %v", err)
				}
				cont.Destroy()
				cont = nil

				// Restore into a new container instance.
				cont2, err := New(&testConf, args)
				if err != nil {
					t.Fatalf("error creating restored container: %v", err)
				}
				defer cont2.Destroy()

				if err := cont2.Restore(&testConf, dir, sandbox.RestoreOpts{}); err != nil {
					t.Fatalf("error restoring container: %v", err)
				}

				if err := waitForProcessCount(cont2, 1); err != nil {
					t.Fatalf("timed out waiting for restored init process: %v", err)
				}

				// Exec peer process to send SIGKILL to PID 1.
				ws, err := execute(&testConf, cont2, "/bin/sh", "-c", "kill -9 1")
				if err != nil {
					t.Fatalf("execute kill -9 1 failed after restore: %v", err)
				}
				if ws.ExitStatus() != 0 {
					t.Fatalf("kill command exited with status %d, want 0", ws.ExitStatus())
				}

				// Verify PID 1 survived the peer SIGKILL.
				time.Sleep(100 * time.Millisecond)
				procs, err := cont2.Processes()
				if err != nil || len(procs) != 1 || procs[0].PID != 1 {
					t.Fatalf("expected PID 1 to survive peer SIGKILL after restore, got err=%v, procs=%s", err, procListToString(procs))
				}

				// Host signal must still be able to kill PID 1.
				if err := cont2.SignalProcess(unix.SIGKILL, 1); err != nil {
					t.Fatalf("failed to send host SIGKILL to restored container: %v", err)
				}
				waitStatus, err := cont2.Wait()
				if err != nil {
					t.Fatalf("failed waiting for restored container after host SIGKILL: %v", err)
				}
				if !killedBySIGKILL(waitStatus) {
					t.Fatalf("expected restored container killed by host SIGKILL, got %v (status=%d)", waitStatus, waitStatus.ExitStatus())
				}
			})

			t.Run("NonePolicyPreservedAfterRestore", func(t *testing.T) {
				testConf := *conf
				testConf.SignalUnkillablePolicy = config.SignalUnkillableNone

				spec, _ := sleepSpecConf(t)
				_, bundleDir, cleanup, err := testutil.SetupContainer(spec, &testConf)
				if err != nil {
					t.Fatalf("error setting up container: %v", err)
				}
				defer cleanup()

				args := Args{
					ID:        testutil.RandomContainerID(),
					Spec:      spec,
					BundleDir: bundleDir,
				}
				cont, err := New(&testConf, args)
				if err != nil {
					t.Fatalf("error creating container: %v", err)
				}
				defer cont.Destroy()
				if err := cont.Start(&testConf); err != nil {
					t.Fatalf("error starting container: %v", err)
				}

				if err := waitForProcessCount(cont, 1); err != nil {
					t.Fatalf("timed out waiting for init process: %v", err)
				}

				// Checkpoint running container.
				dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-restore-unkillable-none")
				if err != nil {
					t.Fatalf("os.MkdirTemp failed: %v", err)
				}
				defer os.RemoveAll(dir)
				if err := os.Chmod(dir, 0777); err != nil {
					t.Fatalf("error chmoding dir: %v", err)
				}

				if err := cont.Checkpoint(&testConf, dir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelFlateBestSpeed}); err != nil {
					t.Fatalf("error checkpointing container: %v", err)
				}
				cont.Destroy()
				cont = nil

				// Restore into a new container instance.
				cont2, err := New(&testConf, args)
				if err != nil {
					t.Fatalf("error creating restored container: %v", err)
				}
				defer cont2.Destroy()

				if err := cont2.Restore(&testConf, dir, sandbox.RestoreOpts{}); err != nil {
					t.Fatalf("error restoring container: %v", err)
				}

				if err := waitForProcessCount(cont2, 1); err != nil {
					t.Fatalf("timed out waiting for restored init process: %v", err)
				}

				// Peer sends SIGKILL; under None policy this kills PID 1.
				_, _ = execute(&testConf, cont2, "/bin/sh", "-c", "kill -9 1")
				ws, err := cont2.Wait()
				if err != nil {
					t.Fatalf("cont2.Wait: %v", err)
				}
				if !killedBySIGKILL(ws) {
					t.Fatalf("expected container killed by SIGKILL under policy=none after restore, got %v (status=%d)", ws, ws.ExitStatus())
				}
			})
		})
	}
}

// testCheckpointRestore creates a container that continuously writes successive
// integers to a file. To test checkpoint and restore functionality, the
// container is checkpointed and the last number printed to the file is
// recorded. Then, it is restored in two new containers and the first number
// printed from these containers is checked. Both should be the next consecutive
// number after the last number from the checkpointed container.
func testCheckpointRestore(t *testing.T, conf *config.Config, compression statefile.CompressionLevel, newSpecWithScript func(string) *specs.Spec) {
	dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
	if err != nil {
		t.Fatalf("os.MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding file: %q, %v", dir, err)
	}

	outputPath := filepath.Join(dir, "output")
	outputFile, err := createWriteableOutputFile(outputPath)
	if err != nil {
		t.Fatalf("error creating output file: %v", err)
	}
	defer outputFile.Close()

	script := fmt.Sprintf("i=0; while true; do echo $i >> %q; sleep 1; i=$((i+1)); done", outputPath)
	spec := newSpecWithScript(script)
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()

	// Create and start the container.
	args := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont.Destroy()
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}

	// Wait until application has ran.
	if err := waitForFileNotEmpty(outputFile); err != nil {
		t.Fatalf("Failed to wait for output file: %v", err)
	}

	// Checkpoint running container; save state into new file.
	if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{Compression: compression}); err != nil {
		t.Fatalf("error checkpointing container to empty file: %v", err)
	}

	// The state file's metadata gives the format of the pages metadata file
	// if the image has one, which it does unless it is compressed.
	md, err := readStateFileMetadata(filepath.Join(dir, checkpointfiles.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	wantFormat := ""
	if compression == statefile.CompressionLevelNone {
		wantFormat = checkpointimage.FormatVersion()
	}
	if got := md[checkpointimage.FormatMetadataKey]; got != wantFormat {
		t.Errorf("state file metadata %s = %q, want %q", checkpointimage.FormatMetadataKey, got, wantFormat)
	}

	lastNum, err := readOutputNum(outputPath, -1)
	if err != nil {
		t.Fatalf("error with outputFile: %v", err)
	}

	// Delete and recreate file before restoring.
	if err := os.Remove(outputPath); err != nil {
		t.Fatalf("error removing file")
	}
	outputFile2, err := createWriteableOutputFile(outputPath)
	if err != nil {
		t.Fatalf("error creating output file: %v", err)
	}
	defer outputFile2.Close()

	// Restore into a new container with different ID (e.g. clone). Keep the
	// initial container running to ensure no conflict with it.
	args2 := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont2, err := New(conf, args2)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont2.Destroy()

	if err := cont2.Restore(conf, dir, sandbox.RestoreOpts{}); err != nil {
		t.Fatalf("error restoring container: %v", err)
	}

	if !cont2.Sandbox.Restored {
		t.Fatalf("sandbox returned wrong value for Sandbox.Restored, got: false, want: true")
	}

	if cont2.Sandbox.Checkpointed {
		t.Fatalf("sandbox returned wrong value for Sandbox.Checkpointed, got: true, want: false")
	}

	// Wait until application has ran.
	if err := waitForFileNotEmpty(outputFile2); err != nil {
		t.Fatalf("Failed to wait for output file: %v", err)
	}

	firstNum, err := readOutputNum(outputPath, 0)
	if err != nil {
		t.Fatalf("error with outputFile: %v", err)
	}

	// Check that lastNum is one less than firstNum and that the container
	// picks up from where it left off.
	if lastNum+1 != firstNum {
		t.Errorf("error numbers not in order, previous: %d, next: %d", lastNum, firstNum)
	}
	cont2.Destroy()
	cont2 = nil

	// Restore into a container using the same ID (e.g. save/resume). It requires
	// the original container to cease to exist because they share the same identity.
	cont.Destroy()
	cont = nil

	// Delete and recreate file before restoring.
	if err := os.Remove(outputPath); err != nil {
		t.Fatalf("error removing file")
	}
	outputFile3, err := createWriteableOutputFile(outputPath)
	if err != nil {
		t.Fatalf("error creating output file: %v", err)
	}
	defer outputFile3.Close()

	cont3, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont3.Destroy()

	if err := cont3.Restore(conf, dir, sandbox.RestoreOpts{}); err != nil {
		t.Fatalf("error restoring container: %v", err)
	}

	// Wait until application has ran.
	if err := waitForFileNotEmpty(outputFile3); err != nil {
		t.Fatalf("Failed to wait for output file: %v", err)
	}

	firstNum2, err := readOutputNum(outputPath, 0)
	if err != nil {
		t.Fatalf("error with outputFile: %v", err)
	}

	// Check that lastNum is one less than firstNum and that the container
	// picks up from where it left off.
	if lastNum+1 != firstNum2 {
		t.Errorf("error numbers not in order, previous: %d, next: %d", lastNum, firstNum2)
	}
	cont3.Destroy()
}

// readStateFileMetadata returns the metadata of the state file at path.
func readStateFileMetadata(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return statefile.MetadataUnsafe(f)
}

// TestCheckpointRestore does the checkpoint/restore test on each platform.
func TestCheckpointRestore(t *testing.T) {
	// Skip overlay because test requires writing to host file.
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			compressionLevels := []statefile.CompressionLevel{
				statefile.CompressionLevelNone,
				statefile.CompressionLevelFlateBestSpeed,
			}
			for _, compression := range compressionLevels {
				t.Run(string(compression), func(t *testing.T) {
					testCheckpointRestore(t, conf, compression, func(script string) *specs.Spec {
						return testutil.NewSpecWithArgs("bash", "-c", script)
					})
				})
			}
		})
	}
}

// TestCheckpointRestoreLayers restores an image whose pages are partly in
// another image: a checkpoint rebased by "runsc image rebase" onto an earlier
// checkpoint of the same container.
func TestCheckpointRestoreLayers(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			testCheckpointRestoreLayers(t, conf)
		})
	}
}

func testCheckpointRestoreLayers(t *testing.T, conf *config.Config) {
	dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
	if err != nil {
		t.Fatalf("os.MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding file: %q, %v", dir, err)
	}
	baseDir := filepath.Join(dir, "base")
	imageDir := filepath.Join(dir, "image")
	for _, d := range []string{baseDir, imageDir} {
		if err := os.Mkdir(d, 0777); err != nil {
			t.Fatal(err)
		}
	}

	outputPath := filepath.Join(dir, "output")
	outputFile, err := createWriteableOutputFile(outputPath)
	if err != nil {
		t.Fatalf("error creating output file: %v", err)
	}
	defer outputFile.Close()
	script := fmt.Sprintf("i=0; while true; do echo $i >> %q; sleep 1; i=$((i+1)); done", outputPath)
	spec := testutil.NewSpecWithArgs("bash", "-c", script)
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()
	args := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont.Destroy()
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}
	if err := waitForFileNotEmpty(outputFile); err != nil {
		t.Fatalf("Failed to wait for output file: %v", err)
	}

	// Checkpoint the container twice: the base, leaving it running, then the
	// image, once it has counted further.
	if err := cont.Checkpoint(conf, baseDir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelNone, Resume: true}); err != nil {
		t.Fatalf("error checkpointing container: %v", err)
	}
	baseNum, err := readOutputNum(outputPath, -1)
	if err != nil {
		t.Fatalf("error with outputFile: %v", err)
	}
	if err := testutil.Poll(func() error {
		n, err := readOutputNum(outputPath, -1)
		if err != nil {
			return err
		}
		if n < baseNum+2 {
			return fmt.Errorf("counted to %d, waiting for %d", n, baseNum+2)
		}
		return nil
	}, pollTimeout); err != nil {
		t.Fatal(err)
	}
	if err := cont.Checkpoint(conf, imageDir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelNone}); err != nil {
		t.Fatalf("error checkpointing container: %v", err)
	}
	lastNum, err := readOutputNum(outputPath, -1)
	if err != nil {
		t.Fatalf("error with outputFile: %v", err)
	}
	cont.Destroy()
	cont = nil

	// runImage runs "runsc image" with args.
	runImage := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(specutils.ExePath, append([]string{"image"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("runsc image %q: %v\n%s", args, err, out)
		}
	}

	// Rebase the image onto the base: most of the container's memory did not
	// change in between, so the rebased image refers to the base's pages.
	rebasedDir := filepath.Join(dir, "rebased")
	runImage("rebase", "--onto", baseDir, "--output", rebasedDir, imageDir)
	base, err := checkpointimage.ReadMetadataFile(filepath.Join(baseDir, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	rebased, err := checkpointimage.ReadMetadataFile(filepath.Join(rebasedDir, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if layers := rebased.Layers(); len(layers) != 2 || layers[1].Digest != base.Digest {
		t.Fatalf("rebased image has layers %+v, want itself and the base %v", layers, base.Digest)
	}
	image, err := checkpointimage.ReadMetadataFile(filepath.Join(imageDir, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pages: image %d bytes, rebased image %d bytes and base %d bytes", image.Layers()[0].PagesSize, rebased.Layers()[0].PagesSize, base.Layers()[0].PagesSize)

	// The image records what restoring it requires, and this host has it.
	out, err := exec.Command(specutils.ExePath, "image", "inspect", "--json", rebasedDir).Output()
	if err != nil {
		t.Fatalf("runsc image inspect: %v", err)
	}
	var info struct {
		State struct {
			Metadata map[string]string `json:"metadata"`
		} `json:"state"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("runsc image inspect printed %q: %v", out, err)
	}
	if md := info.State.Metadata; md[boot.PlatformKey] != conf.Platform || md[sentryapi.CPUFeaturesKey] == "" || md[checkpointimage.FormatMetadataKey] != checkpointimage.FormatVersion() {
		t.Errorf("image metadata %v lacks platform %q, CPU features or image format %s", md, conf.Platform, checkpointimage.FormatVersion())
	}
	if out, err := exec.Command(specutils.ExePath, "image", "verify", "--host", "--pages", "--layer-path", baseDir, rebasedDir).CombinedOutput(); err != nil {
		t.Errorf("runsc image verify --host: %v\n%s", err, out)
	}
	// Without the layer path, verify exits with the status for a missing
	// layer, 4.
	var exitErr *exec.ExitError
	if out, err := exec.Command(specutils.ExePath, "image", "verify", rebasedDir).CombinedOutput(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 4 {
		t.Errorf("runsc image verify without the layer: %v, want exit status 4\n%s", err, out)
	}

	// restore restores the image in imageDir into a new container, which it
	// returns, and checks that it counts on from lastNum.
	restore := func(imageDir string, layerPaths []string, background bool, lastNum int) (*Container, error) {
		t.Helper()
		if err := os.Remove(outputPath); err != nil {
			t.Fatalf("error removing file: %v", err)
		}
		outputFile, err := createWriteableOutputFile(outputPath)
		if err != nil {
			t.Fatalf("error creating output file: %v", err)
		}
		defer outputFile.Close()
		cont, err := New(conf, Args{
			ID:        testutil.RandomContainerID(),
			Spec:      spec,
			BundleDir: bundleDir,
		})
		if err != nil {
			t.Fatalf("error creating container: %v", err)
		}
		t.Cleanup(func() { cont.Destroy() })
		if err := cont.Restore(conf, imageDir, sandbox.RestoreOpts{LayerPaths: layerPaths, Background: background}); err != nil {
			cont.Destroy()
			return nil, err
		}
		if err := waitForFileNotEmpty(outputFile); err != nil {
			t.Fatalf("Failed to wait for output file: %v", err)
		}
		firstNum, err := readOutputNum(outputPath, 0)
		if err != nil {
			t.Fatalf("error with outputFile: %v", err)
		}
		if lastNum+1 != firstNum {
			t.Errorf("error numbers not in order, previous: %d, next: %d", lastNum, firstNum)
		}
		return cont, nil
	}

	// Without its layer, the image cannot be restored.
	if _, err := restore(rebasedDir, nil, false /* background */, lastNum); err == nil {
		t.Errorf("restoring the rebased image without its layer succeeded")
	}
	// The layer given by --layer-path. The restored container is checkpointed
	// again once it has counted on: the image of a chain of three.
	restored, err := restore(rebasedDir, []string{baseDir}, false /* background */, lastNum)
	if err != nil {
		t.Fatalf("error restoring container with layer path %q: %v", baseDir, err)
	}
	if err := testutil.Poll(func() error {
		if n, err := readOutputNum(outputPath, -1); err != nil || n < lastNum+2 {
			return fmt.Errorf("counted to %d (%v), waiting for %d", n, err, lastNum+2)
		}
		return nil
	}, pollTimeout); err != nil {
		t.Fatal(err)
	}
	image2Dir := filepath.Join(dir, "image2")
	if err := os.Mkdir(image2Dir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := restored.Checkpoint(conf, image2Dir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelNone}); err != nil {
		t.Fatalf("error checkpointing the restored container: %v", err)
	}
	lastNum2, err := readOutputNum(outputPath, -1)
	if err != nil {
		t.Fatalf("error with outputFile: %v", err)
	}
	restored.Destroy()

	// The layer in the image's layers directory, loaded in the background.
	layerDir := filepath.Join(rebasedDir, checkpointimage.LayersDir, base.Digest.String())
	if err := os.MkdirAll(filepath.Dir(layerDir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(baseDir, layerDir); err != nil {
		t.Fatal(err)
	}
	if restored, err := restore(rebasedDir, nil, true /* background */, lastNum); err != nil {
		t.Errorf("error restoring container with its layer in %q: %v", layerDir, err)
	} else {
		restored.Destroy()
	}

	// The second image, rebased onto the rebased image, refers to the base
	// and to the rebased image for the pages that changed between the base
	// and the first image and stayed the same since.
	rebased2Dir := filepath.Join(dir, "rebased2")
	runImage("rebase", "--onto", rebasedDir, "--output", rebased2Dir, image2Dir)
	rebased2, err := checkpointimage.ReadMetadataFile(filepath.Join(rebased2Dir, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("second image rebased onto the rebased image: layers %+v", rebased2.Layers())

	// Flattened, it needs no layer.
	flatDir := filepath.Join(dir, "flat")
	runImage("flatten", "--layer-path", baseDir, "--layer-path", rebasedDir, "--output", flatDir, rebased2Dir)
	if restored, err := restore(flatDir, nil, false /* background */, lastNum2); err != nil {
		t.Errorf("error restoring the flattened image: %v", err)
	} else {
		restored.Destroy()
	}

	// Compacted onto the base, it refers to the base only, and has the data
	// of the other layers in its own pages file.
	compactDir := filepath.Join(dir, "compacted")
	runImage("compact", "--keep-layer", base.Digest.String(), "--layer-path", baseDir, "--layer-path", rebasedDir, "--output", compactDir, rebased2Dir)
	compacted, err := checkpointimage.ReadMetadataFile(filepath.Join(compactDir, checkpointfiles.PagesMetadataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if layers := compacted.Layers(); len(layers) != 2 || layers[1].Digest != base.Digest {
		t.Errorf("compacted image has layers %+v, want itself and the base %v", layers, base.Digest)
	}
	if restored, err := restore(compactDir, []string{baseDir}, true /* background */, lastNum2); err != nil {
		t.Errorf("error restoring the compacted image: %v", err)
	} else {
		restored.Destroy()
	}

	// runsc, run as checkpointctl-runsc, is a checkpointctl plugin that reads
	// the checkpoint archives that container engines make, here containerd's
	// of the second checkpoint.
	plugin := filepath.Join(dir, "checkpointctl-runsc")
	if err := os.Symlink(specutils.ExePath, plugin); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plugin, "--plugin-description").Output(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 || len(out) == 0 {
		t.Errorf("%s --plugin-description: %v, %q; want a description and exit status 42", plugin, err, out)
	}
	archivePath := filepath.Join(dir, "checkpoint.tar")
	writeCheckpointArchive(t, archivePath, image2Dir, spec)
	if out, err := exec.Command(plugin, "verify", "--pages", archivePath).CombinedOutput(); err != nil {
		t.Errorf("%s verify --pages: %v\n%s", plugin, err, out)
	}
	out, err = exec.Command(plugin, "inspect", "--json", archivePath).Output()
	if err != nil {
		t.Fatalf("%s inspect: %v", plugin, err)
	}
	var archiveInfo struct {
		Archive struct {
			Container struct {
				Name   string `json:"name"`
				Engine string `json:"engine"`
			} `json:"container"`
		} `json:"archive"`
	}
	if err := json.Unmarshal(out, &archiveInfo); err != nil {
		t.Fatalf("%s inspect printed %q: %v", plugin, out, err)
	}
	if c := archiveInfo.Archive.Container; c.Name != "counter" || c.Engine != "containerd" {
		t.Errorf("%s inspect described the container as %+v, want containerd's counter", plugin, c)
	}
}

// writeCheckpointArchive writes at path a checkpoint archive as containerd
// makes one of a container with the OCI spec spec: the image in the directory
// image as its checkpoint/ directory, the spec as spec.dump, and containerd's
// description of the container as config.dump.
func writeCheckpointArchive(t *testing.T, path, image string, spec *specs.Spec) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	add := func(name string, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	dump := *spec
	dump.Annotations = map[string]string{"io.kubernetes.cri.container-name": "counter"}
	for name, v := range map[string]any{
		"spec.dump":   &dump,
		"config.dump": map[string]string{"name": "counter", "runtime": "io.containerd.runsc.v1"},
	} {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		add(name, data)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "checkpoint/", Typeflag: tar.TypeDir, Mode: 0700}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{checkpointfiles.StateFileName, checkpointfiles.PagesMetadataFileName, checkpointfiles.PagesFileName} {
		src, err := os.Open(filepath.Join(image, name))
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		st, err := src.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "checkpoint/" + name, Typeflag: tar.TypeReg, Mode: 0600, Size: st.Size()}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tw, src); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCheckpointRestoreClocks verifies that CLOCK_MONOTONIC does not count the
// time that the sandbox spends checkpointed, so that a sleep pending at
// checkpoint time keeps the time it had left, while CLOCK_REALTIME and
// CLOCK_BOOTTIME do count it.
func TestCheckpointRestoreClocks(t *testing.T) {
	const (
		sleepDuration        = 4 * time.Second
		checkpointedDuration = 8 * time.Second
	)
	app, err := testutil.FindFile("test/cmd/test_app/test_app")
	if err != nil {
		t.Fatalf("error finding test_app: %v", err)
	}
	// Skip overlay because test requires writing to host file.
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
			if err != nil {
				t.Fatalf("os.MkdirTemp failed: %v", err)
			}
			defer os.RemoveAll(dir)
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatalf("error chmoding dir: %q, %v", dir, err)
			}
			beforePath := filepath.Join(dir, "before")
			afterPath := filepath.Join(dir, "after")

			// Record the clocks around a sleep(1), which glibc implements
			// with a relative CLOCK_REALTIME clock_nanosleep(2). Rename the
			// files into place so that they are never read partially
			// written.
			script := fmt.Sprintf("%[1]s clocks > %[2]s.tmp && mv %[2]s.tmp %[2]s && sleep %[4]d && %[1]s clocks > %[3]s.tmp && mv %[3]s.tmp %[3]s",
				app, beforePath, afterPath, int(sleepDuration.Seconds()))
			spec := testutil.NewSpecWithArgs("bash", "-c", script)
			_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
			if err != nil {
				t.Fatalf("error setting up container: %v", err)
			}
			defer cleanup()

			args := Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			}
			cont, err := New(conf, args)
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont.Destroy()
			if err := cont.Start(conf); err != nil {
				t.Fatalf("error starting container: %v", err)
			}
			if err := waitForFileExist(beforePath); err != nil {
				t.Fatalf("error waiting for %q: %v", beforePath, err)
			}
			if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{}); err != nil {
				t.Fatalf("error checkpointing container: %v", err)
			}
			if _, err := os.Stat(afterPath); err == nil {
				t.Fatalf("sleep finished before checkpoint")
			}
			cont.Destroy()

			time.Sleep(checkpointedDuration)

			cont2, err := New(conf, args)
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont2.Destroy()
			if err := cont2.Restore(conf, dir, sandbox.RestoreOpts{}); err != nil {
				t.Fatalf("error restoring container: %v", err)
			}
			if err := waitForFileExist(afterPath); err != nil {
				t.Fatalf("error waiting for %q: %v", afterPath, err)
			}

			readClocks := func(path string) (realtime, monotonic, boottime time.Duration) {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("error reading %q: %v", path, err)
				}
				if _, err := fmt.Sscan(string(b), &realtime, &monotonic, &boottime); err != nil {
					t.Fatalf("error parsing %q from %q: %v", b, path, err)
				}
				return realtime, monotonic, boottime
			}
			realtime1, monotonic1, boottime1 := readClocks(beforePath)
			realtime2, monotonic2, boottime2 := readClocks(afterPath)
			realtime := realtime2 - realtime1
			monotonic := monotonic2 - monotonic1
			boottime := boottime2 - boottime1
			t.Logf("Elapsed time: CLOCK_REALTIME %v, CLOCK_MONOTONIC %v, CLOCK_BOOTTIME %v", realtime, monotonic, boottime)

			// The sleep resumed with the time it had left: it neither
			// expired on restore, nor counted the time spent checkpointed.
			if monotonic < sleepDuration || monotonic >= checkpointedDuration {
				t.Errorf("CLOCK_MONOTONIC advanced by %v during the sleep, want in [%v, %v)", monotonic, sleepDuration, checkpointedDuration)
			}
			// The difference also includes the time between the checkpoint
			// and restore requests and the actual checkpoint and restore.
			if got := realtime - monotonic; got < checkpointedDuration {
				t.Errorf("CLOCK_REALTIME advanced by %v more than CLOCK_MONOTONIC, want at least %v", got, checkpointedDuration)
			}
			if got := boottime - monotonic; got < checkpointedDuration {
				t.Errorf("CLOCK_BOOTTIME advanced by %v more than CLOCK_MONOTONIC, want at least %v", got, checkpointedDuration)
			}
			if diff := boottime - realtime; diff < -time.Second || diff > time.Second {
				t.Errorf("CLOCK_BOOTTIME advanced by %v, want within 1s of CLOCK_REALTIME's %v", boottime, realtime)
			}
		})
	}
}

// TestCheckpointCPUFeaturesMetadata verifies that the checkpoint metadata
// records the sandbox's CPU features, matching the CPUID FeatureSet saved in
// the state file.
func TestCheckpointCPUFeaturesMetadata(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			// Use the wire engine because this test decodes cpuid.FeatureSet
			// directly in-process without linking all Sentry state types.
			conf.StateEngine = config.StateEngineWire
			spec, _ := sleepSpecConf(t)
			_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
			if err != nil {
				t.Fatalf("error setting up container: %v", err)
			}
			defer cleanup()

			cont, err := New(conf, Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			})
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont.Destroy()
			if err := cont.Start(conf); err != nil {
				t.Fatalf("error starting container: %v", err)
			}
			if err := waitForProcessCount(cont, 1); err != nil {
				t.Fatalf("error waiting for init process: %v", err)
			}

			dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-cpu-features")
			if err != nil {
				t.Fatalf("os.MkdirTemp failed: %v", err)
			}
			defer os.RemoveAll(dir)
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatalf("error chmoding dir: %v", err)
			}
			if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelFlateBestSpeed}); err != nil {
				t.Fatalf("error checkpointing container: %v", err)
			}

			f, err := os.Open(filepath.Join(dir, checkpointfiles.StateFileName))
			if err != nil {
				t.Fatalf("error opening state file: %v", err)
			}
			defer f.Close()
			r, metadata, err := statefile.NewReader(f, nil /* key */)
			if err != nil {
				t.Fatalf("error reading state file: %v", err)
			}
			defer r.Close()

			got, ok := metadata[sentryapi.CPUFeaturesKey]
			if !ok {
				t.Fatalf("metadata key %q not found", sentryapi.CPUFeaturesKey)
			}
			if got == "" {
				t.Fatalf("metadata[%q] is empty", sentryapi.CPUFeaturesKey)
			}

			// Kernel.saveToLocked saves the CPUID FeatureSet as the first object in
			// the state stream (which Kernel.LoadFrom also relies on).
			var fs cpuid.FeatureSet
			if _, err := state.Load(context.Background(), r, &fs); err != nil {
				t.Fatalf("error loading CPUID FeatureSet from state file: %v", err)
			}
			if want := strings.Join(fs.FeatureNames(), ","); got != want {
				t.Errorf("metadata[%q] = %q, want %q", sentryapi.CPUFeaturesKey, got, want)
			}
			gotMask, err := cpuid.FeatureMaskStringFromNames(strings.Split(got, ","))
			if err != nil {
				t.Fatalf("FeatureMaskStringFromNames(%q) failed: %v", got, err)
			}
			if wantMask := fs.FeatureMaskString(); gotMask != wantMask {
				t.Errorf("FeatureMaskStringFromNames(%q) = %q, want %q", got, gotMask, wantMask)
			}
		})
	}
}

// TestCheckpointRestoreBackground checks that a sandbox restored with
// --background, whose memory loads while its application runs, finds its
// memory intact: the application checksums 32 MiB of random data that it holds
// in memory before the checkpoint and after the restore.
func TestCheckpointRestoreBackground(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
			if err != nil {
				t.Fatalf("os.MkdirTemp failed: %v", err)
			}
			defer os.RemoveAll(dir)
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatalf("error chmoding file: %q, %v", dir, err)
			}
			testCheckpointRestoreBackground(t, conf, dir)
		})
	}
}

// TestCheckpointRestoreS3 checks that a container checkpointed to an
// S3-compatible object store through the checkpoint gofer, uncompressed,
// restores from it with --background.
func TestCheckpointRestoreS3(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			store := s3test.NewStore(t)
			srv := httptest.NewServer(store)
			defer srv.Close()
			dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
			if err != nil {
				t.Fatalf("os.MkdirTemp failed: %v", err)
			}
			defer os.RemoveAll(dir)
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatalf("error chmoding file: %q, %v", dir, err)
			}
			opts := fmt.Sprintf(`{"endpoint": %q, "bucket": %q, "object_prefix": "image/", "credentials": {"access_key_id": "key", "secret_access_key": "secret"}}`, srv.URL, s3test.Bucket)
			if err := os.WriteFile(filepath.Join(dir, "s3_opts.json"), []byte(opts), 0600); err != nil {
				t.Fatalf("error writing S3 options: %v", err)
			}

			testCheckpointRestoreBackground(t, conf, dir)

			for _, name := range []string{checkpointfiles.StateFileName, checkpointfiles.PagesMetadataFileName, checkpointfiles.PagesFileName} {
				if _, ok := store.Object("image/" + name); !ok {
					t.Errorf("checkpoint did not write %s to the object store", name)
				}
				if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
					t.Errorf("checkpoint wrote %s to the image path", name)
				}
			}
		})
	}
}

// testCheckpointRestoreBackground checkpoints a container that holds 32 MiB of
// data to imagePath, uncompressed, restores it with --background, and checks
// that its data survived.
func testCheckpointRestoreBackground(t *testing.T, conf *config.Config, imagePath string) {
	outputPath := filepath.Join(imagePath, "output")
	outputFile, err := createWriteableOutputFile(outputPath)
	if err != nil {
		t.Fatalf("error creating output file: %v", err)
	}
	defer outputFile.Close()

	script := fmt.Sprintf(`x=$(head -c %d /dev/urandom | base64 -w0); while true; do echo -n "$x" | md5sum >> %q; sleep 0.2; done`, 32<<20, outputPath)
	spec := testutil.NewSpecWithArgs("bash", "-c", script)
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()
	args := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont.Destroy()
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}
	if err := waitForFileNotEmpty(outputFile); err != nil {
		t.Fatalf("Failed to wait for output file: %v", err)
	}
	want, err := firstLine(outputPath)
	if err != nil {
		t.Fatalf("error reading output: %v", err)
	}

	if err := cont.Checkpoint(conf, imagePath, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelNone}); err != nil {
		t.Fatalf("error checkpointing container: %v", err)
	}
	cont.Destroy()
	cont = nil
	if err := os.Remove(outputPath); err != nil {
		t.Fatalf("error removing file: %v", err)
	}
	outputFile2, err := createWriteableOutputFile(outputPath)
	if err != nil {
		t.Fatalf("error creating output file: %v", err)
	}
	defer outputFile2.Close()

	cont2, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont2.Destroy()
	if err := cont2.Restore(conf, imagePath, sandbox.RestoreOpts{Background: true}); err != nil {
		t.Fatalf("error restoring container: %v", err)
	}
	if err := waitForFileNotEmpty(outputFile2); err != nil {
		t.Fatalf("Failed to wait for output file: %v", err)
	}
	got, err := firstLine(outputPath)
	if err != nil {
		t.Fatalf("error reading output: %v", err)
	}
	if got != want {
		t.Errorf("checksum of the application's data after the restore is %q, want %q as before the checkpoint", got, want)
	}
}

// firstLine returns the first complete line of the file at path.
func firstLine(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, _, ok := strings.Cut(string(data), "\n")
	if !ok {
		return "", fmt.Errorf("no complete line in %q", data)
	}
	return line, nil
}

// TestCheckpointRestoreBackgroundSlowPagesFile checks that a sandbox restored
// with --background from a slow pages file answers long before its memory is
// loaded, and finds its memory intact. The application holds 64 MiB of random
// data in base64, and answers by appending a line to a file every 50 ms, which
// touches little of its memory; asked to, it checksums its data. The restore
// reads its pages file at the rate that loads it in 8 seconds, as a disk
// throttled by cgroup io.max would (which these tests cannot set up, since
// they run unprivileged): its first answer must come within 3 seconds.
func TestCheckpointRestoreBackgroundSlowPagesFile(t *testing.T) {
	const (
		loadTime = 8 * time.Second
		maxWait  = 3 * time.Second
	)
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
			if err != nil {
				t.Fatalf("os.MkdirTemp failed: %v", err)
			}
			defer os.RemoveAll(dir)
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatalf("error chmoding file: %q, %v", dir, err)
			}
			imagePath := filepath.Join(dir, "image")
			if err := os.Mkdir(imagePath, 0777); err != nil {
				t.Fatalf("os.Mkdir failed: %v", err)
			}
			answersPath := filepath.Join(dir, "answers")
			checkPath := filepath.Join(dir, "check")
			sumPath := filepath.Join(dir, "sum")
			script := fmt.Sprintf(`x=$(head -c %d /dev/urandom | base64 -w0)
echo -n "$x" | md5sum > %[2]q.tmp && mv %[2]q.tmp %[2]q
while true; do
  if [ -e %[3]q ]; then rm %[3]q; echo -n "$x" | md5sum > %[2]q.tmp && mv %[2]q.tmp %[2]q; fi
  echo >> %[4]q
  sleep 0.05
done`, 64<<20, sumPath, checkPath, answersPath)
			spec := testutil.NewSpecWithArgs("bash", "-c", script)
			_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
			if err != nil {
				t.Fatalf("error setting up container: %v", err)
			}
			defer cleanup()
			args := Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			}
			// sum returns the checksum of the application's data, waiting up
			// to timeout for it to be written.
			sum := func(timeout time.Duration) string {
				t.Helper()
				var line string
				if err := testutil.Poll(func() error {
					var err error
					line, err = firstLine(sumPath)
					return err
				}, timeout); err != nil {
					t.Fatalf("error waiting for the checksum of the application's data: %v", err)
				}
				return line
			}

			cont, err := New(conf, args)
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont.Destroy()
			if err := cont.Start(conf); err != nil {
				t.Fatalf("error starting container: %v", err)
			}
			want := sum(time.Minute)
			if err := cont.Checkpoint(conf, imagePath, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelNone}); err != nil {
				t.Fatalf("error checkpointing container: %v", err)
			}
			cont.Destroy()
			for _, p := range []string{answersPath, sumPath} {
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					t.Fatalf("error removing file: %v", err)
				}
			}

			pages, err := os.Stat(filepath.Join(imagePath, checkpointfiles.PagesFileName))
			if err != nil {
				t.Fatalf("error reading the size of the pages file: %v", err)
			}
			conf.TestOnlyRestoreReadRate = uint64(pages.Size()) * uint64(time.Second) / uint64(loadTime)
			t.Logf("pages file of %d bytes, read at %d bytes/s", pages.Size(), conf.TestOnlyRestoreReadRate)
			answers, err := createWriteableOutputFile(answersPath)
			if err != nil {
				t.Fatalf("error creating file: %v", err)
			}
			defer answers.Close()
			cont2, err := New(conf, args)
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont2.Destroy()
			start := time.Now()
			if err := cont2.Restore(conf, imagePath, sandbox.RestoreOpts{Background: true}); err != nil {
				t.Fatalf("error restoring container: %v", err)
			}
			if err := waitForFileNotEmpty(answers); err != nil {
				t.Fatalf("error waiting for an answer: %v", err)
			}
			if wait := time.Since(start); wait > maxWait {
				t.Errorf("first answer came %v after the restore started, want at most %v (loading every page takes %v)", wait, maxWait, loadTime)
			} else {
				t.Logf("first answer came %v after the restore started", wait)
			}
			check, err := createWriteableOutputFile(checkPath)
			if err != nil {
				t.Fatalf("error creating file: %v", err)
			}
			check.Close()
			if got := sum(loadTime + time.Minute); got != want {
				t.Errorf("checksum of the application's data after the restore is %q, want %q as before the checkpoint", got, want)
			}
		})
	}
}

// TestCheckpointRestoreBackgroundUffd checks that the pages that async page
// loading writes after a restore with --background are not reported dirty by
// --dirty-tracking=uffd, which starts tracking while they load: they are the
// image the sandbox was restored from, not writes. The application holds 32
// MiB of random data in base64; it is checkpointed, then restored with
// --background from a pages file read at the rate that loads it in 4
// seconds, and checksums its data, which waits for every page of it to load.
// Its next checkpoint is incremental, relative to the image it was restored
// from, and pre-copies: pre-copy writes every page reported dirty to the
// pages file, without comparing it with the parent's as the final save does,
// so the pages file must hold only the few pages the application wrote. The
// restore from both checkpoints must find the data intact. Verification
// (--dirty-tracking-verify=hash) would make the restore wait for every page
// before tracking starts, which is the case this test must not avoid; the
// last restore checks the incremental checkpoint's contents instead.
func TestCheckpointRestoreBackgroundUffd(t *testing.T) {
	const loadTime = 4 * time.Second
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			requireWriteTracking(t)
			conf.DirtyTracking = config.DirtyTrackingUFFD
			dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
			if err != nil {
				t.Fatalf("os.MkdirTemp failed: %v", err)
			}
			// Removed by cleanups rather than deferred calls, as the
			// containers that restore creates are destroyed by cleanups,
			// which run after deferred calls.
			t.Cleanup(func() { os.RemoveAll(dir) })
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatalf("error chmoding file: %q, %v", dir, err)
			}
			baseDir := filepath.Join(dir, "base")
			incrementalDir := filepath.Join(dir, "incremental")
			for _, d := range []string{baseDir, incrementalDir} {
				if err := os.Mkdir(d, 0777); err != nil {
					t.Fatalf("os.Mkdir failed: %v", err)
				}
			}
			checkPath := filepath.Join(dir, "check")
			sumPath := filepath.Join(dir, "sum")
			script := fmt.Sprintf(`x=$(head -c %d /dev/urandom | base64 -w0)
echo -n "$x" | md5sum > %[2]q.tmp && mv %[2]q.tmp %[2]q
while true; do
  if [ -e %[3]q ]; then rm %[3]q; echo -n "$x" | md5sum > %[2]q.tmp && mv %[2]q.tmp %[2]q; fi
  sleep 0.05
done`, 32<<20, sumPath, checkPath)
			spec := testutil.NewSpecWithArgs("bash", "-c", script)
			_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
			if err != nil {
				t.Fatalf("error setting up container: %v", err)
			}
			t.Cleanup(cleanup)
			args := Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			}
			// sum returns the checksum of the application's data, after
			// asking for it if ask is true, waiting up to timeout for it.
			sum := func(ask bool, timeout time.Duration) string {
				t.Helper()
				if ask {
					check, err := createWriteableOutputFile(checkPath)
					if err != nil {
						t.Fatalf("error creating file: %v", err)
					}
					check.Close()
				}
				var line string
				if err := testutil.Poll(func() error {
					if _, err := os.Stat(checkPath); err == nil {
						return fmt.Errorf("the application has not checksummed its data yet")
					}
					var err error
					line, err = firstLine(sumPath)
					return err
				}, timeout); err != nil {
					t.Fatalf("error waiting for the checksum of the application's data: %v", err)
				}
				if err := os.Remove(sumPath); err != nil {
					t.Fatalf("error removing file: %v", err)
				}
				return line
			}
			// restore restores a new container from the checkpoint in
			// imageDir, in the background, reading pages files at
			// readRate bytes per second if it is not 0, and checks that
			// the application's data is intact.
			restore := func(imageDir string, layerPaths []string, readRate uint64, want string) *Container {
				t.Helper()
				conf.TestOnlyRestoreReadRate = readRate
				cont, err := New(conf, Args{
					ID:        testutil.RandomContainerID(),
					Spec:      spec,
					BundleDir: bundleDir,
				})
				if err != nil {
					t.Fatalf("error creating container: %v", err)
				}
				t.Cleanup(func() { cont.Destroy() })
				if err := cont.Restore(conf, imageDir, sandbox.RestoreOpts{LayerPaths: layerPaths, Background: true}); err != nil {
					t.Fatalf("error restoring container: %v", err)
				}
				if got := sum(true, loadTime+time.Minute); got != want {
					t.Fatalf("checksum of the application's data after the restore from %s is %q, want %q as before the checkpoint", imageDir, got, want)
				}
				return cont
			}

			cont, err := New(conf, args)
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont.Destroy()
			if err := cont.Start(conf); err != nil {
				t.Fatalf("error starting container: %v", err)
			}
			want := sum(false, time.Minute)
			if err := cont.Checkpoint(conf, baseDir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelNone}); err != nil {
				t.Fatalf("error checkpointing container: %v", err)
			}
			cont.Destroy()

			base, err := os.Stat(filepath.Join(baseDir, checkpointfiles.PagesFileName))
			if err != nil {
				t.Fatalf("error reading the size of the pages file: %v", err)
			}
			cont2 := restore(baseDir, nil /* layerPaths */, uint64(base.Size())*uint64(time.Second)/uint64(loadTime), want)
			if err := cont2.Checkpoint(conf, incrementalDir, sandbox.CheckpointOpts{
				Compression:      statefile.CompressionLevelNone,
				ParentImagePath:  baseDir,
				Precopy:          "on",
				PrecopyBudget:    100 * time.Millisecond,
				PrecopyMaxRounds: 8,
			}); err != nil {
				t.Fatalf("error checkpointing the restored container incrementally: %v", err)
			}
			cont2.Destroy()
			incremental, err := os.Stat(filepath.Join(incrementalDir, checkpointfiles.PagesFileName))
			if err != nil {
				t.Fatalf("error reading the size of the pages file: %v", err)
			}
			t.Logf("pages files: %d bytes in the checkpoint, %d bytes in the incremental checkpoint of its restore", base.Size(), incremental.Size())
			if incremental.Size() > base.Size()/8 {
				t.Errorf("the incremental checkpoint of the restored container holds %d bytes of pages, want at most an eighth of its parent's %d: the pages loaded in the background were reported dirty", incremental.Size(), base.Size())
			}

			restore(incrementalDir, []string{baseDir}, 0 /* readRate */, want)
		})
	}
}

// TestCheckpointRestorePrecopy checks checkpoints that pre-copy memory while
// the container runs. The container tracks dirty pages, with each dirty source
// on each platform, with verification: its first checkpoint, which starts
// tracking while it runs, or an incremental checkpoint of a full one,
// pre-copies; then the container is restored from the checkpoint (and its
// parent) without dirty tracking, and continues where it stopped.
func TestCheckpointRestorePrecopy(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		for _, source := range []config.DirtyTrackingMode{config.DirtyTrackingWriteProtect, config.DirtyTrackingUFFD} {
			for _, incremental := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/dirty-tracking=%v/incremental=%t", name, source, incremental), func(t *testing.T) {
					if source == config.DirtyTrackingUFFD {
						requireWriteTracking(t)
					}
					conf := *conf
					conf.DirtyTracking = source
					conf.DirtyTrackingVerify = config.DirtyTrackingVerifyHash
					testCheckpointRestorePrecopy(t, &conf, incremental)
				})
			}
		}
	}
}

// requireWriteTracking ends the test (hostmmtest.Unavailable) if the host
// cannot track writes as --dirty-tracking=uffd does.
func requireWriteTracking(t *testing.T) {
	t.Helper()
	uffd, err := hostmm.NewWPAsyncUserfaultfd()
	if err != nil {
		hostmmtest.Unavailable(t, err)
	}
	unix.Close(uffd)
}

func testCheckpointRestorePrecopy(t *testing.T, conf *config.Config, incremental bool) {

	dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
	if err != nil {
		t.Fatalf("os.MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding file: %q, %v", dir, err)
	}
	outputPath := filepath.Join(dir, "output")
	outputFile, err := createWriteableOutputFile(outputPath)
	if err != nil {
		t.Fatalf("error creating output file: %v", err)
	}
	defer outputFile.Close()

	script := fmt.Sprintf("i=0; while true; do echo $i >> %q; sleep 0.1; i=$((i+1)); done", outputPath)
	spec := testutil.NewSpecWithArgs("bash", "-c", script)
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()
	cont, err := New(conf, Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	})
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont.Destroy()
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}
	if err := waitForFileNotEmpty(outputFile); err != nil {
		t.Fatalf("Failed to wait for output file: %v", err)
	}

	opts := sandbox.CheckpointOpts{
		Compression:      statefile.CompressionLevelNone,
		Precopy:          "on",
		PrecopyBudget:    100 * time.Millisecond,
		PrecopyMaxRounds: 8,
	}
	var layerPaths []string
	if incremental {
		parentDir := filepath.Join(dir, "parent")
		if err := os.Mkdir(parentDir, 0755); err != nil {
			t.Fatalf("os.Mkdir failed: %v", err)
		}
		if err := cont.Checkpoint(conf, parentDir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelNone, Resume: true}); err != nil {
			t.Fatalf("error checkpointing container: %v", err)
		}
		opts.ParentImagePath = parentDir
		layerPaths = []string{parentDir}
	}
	imageDir := filepath.Join(dir, "image")
	if err := os.Mkdir(imageDir, 0755); err != nil {
		t.Fatalf("os.Mkdir failed: %v", err)
	}
	if err := cont.Checkpoint(conf, imageDir, opts); err != nil {
		t.Fatalf("error checkpointing container with pre-copy: %v", err)
	}
	lastNum, err := readOutputNum(outputPath, -1)
	if err != nil {
		t.Fatalf("error with outputFile: %v", err)
	}
	cont.Destroy()

	if err := os.Remove(outputPath); err != nil {
		t.Fatalf("error removing file")
	}
	outputFile2, err := createWriteableOutputFile(outputPath)
	if err != nil {
		t.Fatalf("error creating output file: %v", err)
	}
	defer outputFile2.Close()
	// Restore without dirty tracking, into the root directory that
	// testutil.SetupContainer gave conf.
	restoreConf := *conf
	restoreConf.DirtyTracking = config.DirtyTrackingOff
	restoreConf.DirtyTrackingVerify = config.DirtyTrackingVerifyOff
	cont2, err := New(&restoreConf, Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	})
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont2.Destroy()
	if err := cont2.Restore(&restoreConf, imageDir, sandbox.RestoreOpts{LayerPaths: layerPaths}); err != nil {
		t.Fatalf("error restoring container: %v", err)
	}
	if err := waitForFileNotEmpty(outputFile2); err != nil {
		t.Fatalf("Failed to wait for output file: %v", err)
	}
	firstNum, err := readOutputNum(outputPath, 0)
	if err != nil {
		t.Fatalf("error with outputFile: %v", err)
	}
	if lastNum+1 != firstNum {
		t.Errorf("error numbers not in order, previous: %d, next: %d", lastNum, firstNum)
	}
}

// TestCheckpointRestoreHostname verifies that hostname is updated on restore
// if it was not changed inside the container, and is NOT updated if it was changed.
func TestCheckpointRestoreHostname(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_%t", changed), func(t *testing.T) {
			spec, conf := sleepSpecConf(t)
			spec.Hostname = "old-host"

			_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
			if err != nil {
				t.Fatalf("error setting up container: %v", err)
			}
			defer cleanup()

			args := Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			}
			cont, err := New(conf, args)
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont.Destroy()
			if err := cont.Start(conf); err != nil {
				t.Fatalf("error starting container: %v", err)
			}

			// Verify initial hostname.
			out, err := executeCombinedOutput(conf, cont, nil, "/bin/sh", "-c", "read -r name < /proc/sys/kernel/hostname && echo $name")
			if err != nil {
				t.Fatalf("exec failed: %v", err)
			}
			if got, want := strings.TrimSpace(string(out)), "old-host"; got != want {
				t.Fatalf("hostname got %q, want %q", got, want)
			}

			if changed {
				// Change hostname inside container.
				_, err = executeCombinedOutput(conf, cont, nil, "/bin/sh", "-c", "echo user-host > /proc/sys/kernel/hostname")
				if err != nil {
					t.Logf("Failed to write to /proc/sys/kernel/hostname: %v, trying hostname command", err)
					_, err = executeCombinedOutput(conf, cont, nil, "/bin/sh", "-c", "hostname user-host")
					if err != nil {
						t.Fatalf("Failed to change hostname: %v", err)
					}
				}

				// Verify it changed.
				out, err = executeCombinedOutput(conf, cont, nil, "/bin/sh", "-c", "read -r name < /proc/sys/kernel/hostname && echo $name")
				if err != nil {
					t.Fatalf("exec failed: %v", err)
				}
				if got, want := strings.TrimSpace(string(out)), "user-host"; got != want {
					t.Fatalf("hostname got %q, want %q", got, want)
				}
			}

			// Checkpoint.
			dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
			if err != nil {
				t.Fatalf("os.MkdirTemp failed: %v", err)
			}
			defer os.RemoveAll(dir)
			if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{}); err != nil {
				t.Fatalf("error checkpointing: %v", err)
			}

			cont.Destroy()

			// Restore with new spec.
			spec2, _ := sleepSpecConf(t)
			spec2.Hostname = "new-host"

			_, bundleDir2, cleanup2, err := testutil.SetupContainer(spec2, conf)
			if err != nil {
				t.Fatalf("error setting up container: %v", err)
			}
			defer cleanup2()

			args2 := Args{
				ID:        args.ID,
				Spec:      spec2,
				BundleDir: bundleDir2,
			}
			cont2, err := New(conf, args2)
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont2.Destroy()

			if err := cont2.Restore(conf, dir, sandbox.RestoreOpts{}); err != nil {
				t.Fatalf("error restoring: %v", err)
			}

			// Verify hostname.
			out, err = executeCombinedOutput(conf, cont2, nil, "/bin/sh", "-c", "read -r name < /proc/sys/kernel/hostname && echo $name")
			if err != nil {
				t.Fatalf("exec failed: %v", err)
			}
			want := "new-host"
			if changed {
				want = "user-host"
			}
			if got, want := strings.TrimSpace(string(out)), want; got != want {
				t.Fatalf("hostname got %q, want %q", got, want)
			}
		})
	}
}

// TestCheckpointRestoreHostinet does the checkpoint/restore test with host
// networking.
func TestCheckpointRestoreHostinet(t *testing.T) {
	app, err := testutil.FindFile("test/cmd/test_app/test_app")
	if err != nil {
		t.Fatal("error finding test_app:", err)
	}

	// Skip overlay because the test app writes its log to a bind-mounted
	// host file.
	for name, conf := range configs(t, true /* noOverlay */) {
		conf.Network = config.NetworkHost
		t.Run(name, func(t *testing.T) {
			testCheckpointRestoreHostinet(t, conf, app)
		})
	}
}

// TestCheckpointResumeHostinet does the checkpoint --leave-running test with
// host networking.
func TestCheckpointResumeHostinet(t *testing.T) {
	app, err := testutil.FindFile("test/cmd/test_app/test_app")
	if err != nil {
		t.Fatal("error finding test_app:", err)
	}

	// Skip overlay because the test app writes its log to a bind-mounted
	// host file.
	for name, conf := range configs(t, true /* noOverlay */) {
		conf.Network = config.NetworkHost
		t.Run(name, func(t *testing.T) {
			testCheckpointResumeHostinet(t, conf, app)
		})
	}
}

// testCheckpointRestoreHostinet checkpoints a hostinet container and checks
// that connected sockets return ECONNABORTED after restore while the restored
// listener keeps accepting.
func testCheckpointRestoreHostinet(t *testing.T, conf *config.Config, app string) {
	dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
	if err != nil {
		t.Fatalf("os.MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding file: %q, %v", dir, err)
	}

	target, connClosed, stopServer := startHostinetSRServer(t)
	defer stopServer()

	logPath := filepath.Join(dir, "hostinet-sr.log")
	spec := testutil.NewSpecWithArgs(app, "hostinet-sr", "--file", logPath, "--target", target)
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()

	args := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont.Destroy()
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}

	if err := waitForHostinetSRLog(logPath, "SETUP_DONE", "TCP_WRITE OK", "UDP_WRITE OK", "EPOLL_WAIT TIMEOUT"); err != nil {
		t.Fatalf("wait for setup: %v", err)
	}
	lastCount, err := lastHostinetSRCount(logPath)
	if err != nil {
		t.Fatalf("lastHostinetSRCount pre-restore: %v", err)
	}
	select {
	case <-connClosed:
		t.Fatalf("server connection closed before checkpoint")
	default:
	}

	if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelFlateBestSpeed}); err != nil {
		t.Fatalf("error checkpointing container: %v", err)
	}

	// The remote peer must observe the connection closing when the
	// checkpointed sandbox exits.
	select {
	case <-connClosed:
	case <-time.After(30 * time.Second):
		t.Fatalf("remote peer did not observe connection close after checkpoint")
	}

	args2 := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont2, err := New(conf, args2)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont2.Destroy()
	if err := cont2.Restore(conf, dir, sandbox.RestoreOpts{}); err != nil {
		t.Fatalf("error restoring container: %v", err)
	}
	if !cont2.Sandbox.Restored {
		t.Fatalf("sandbox returned wrong value for Sandbox.Restored, got: false, want: true")
	}

	if err := waitForHostinetSRLog(logPath,
		"TCP_WRITE ERRNO=32",
		"TCP_READ ERRNO=104",
		"UDP_WRITE ERRNO=104",
		"UDP_READ ERRNO=104",
		"EPOLL_EVENT_ERR",
		"EPOLL_EVENT_HUP",
		"SO_ERROR=104",
		"NEW_TCP_WRITE OK"); err != nil {
		t.Fatalf("wait for post-restore socket state: %v", err)
	}
	if err := waitForHostinetSRCountAfter(logPath, lastCount); err != nil {
		t.Fatalf("wait for post-restore progress: %v", err)
	}

	// Dialing the re-created listener must wake the blocked accept.
	listenerAddr, err := hostinetSRListenerAddr(logPath)
	if err != nil {
		t.Fatalf("hostinetSRListenerAddr: %v", err)
	}
	acceptConn, err := net.Dial("tcp", listenerAddr)
	if err != nil {
		t.Fatalf("dialing restored listener %q: %v", listenerAddr, err)
	}
	defer acceptConn.Close()
	if err := waitForHostinetSRLog(logPath, "BLOCKING_ACCEPT OK"); err != nil {
		t.Fatalf("wait for restored listener accept: %v", err)
	}
}

// testCheckpointResumeHostinet checks that checkpoint with Resume leaves the
// container's sockets working.
func testCheckpointResumeHostinet(t *testing.T, conf *config.Config, app string) {
	dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
	if err != nil {
		t.Fatalf("os.MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding file: %q, %v", dir, err)
	}

	target, connClosed, stopServer := startHostinetSRServer(t)
	defer stopServer()

	logPath := filepath.Join(dir, "hostinet-sr.log")
	spec := testutil.NewSpecWithArgs(app, "hostinet-sr", "--file", logPath, "--target", target)
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()

	args := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont.Destroy()
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}

	if err := waitForHostinetSRLog(logPath, "SETUP_DONE", "TCP_WRITE OK", "UDP_WRITE OK", "EPOLL_WAIT TIMEOUT"); err != nil {
		t.Fatalf("wait for setup: %v", err)
	}
	lastCount, err := lastHostinetSRCount(logPath)
	if err != nil {
		t.Fatalf("lastHostinetSRCount pre-checkpoint: %v", err)
	}

	if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{
		Compression: statefile.CompressionLevelFlateBestSpeed,
		Resume:      true,
	}); err != nil {
		t.Fatalf("error checkpointing container: %v", err)
	}

	// Wait for a few more iterations and check that no socket died. Idle
	// nonblocking reads log EAGAIN, so check specifically for EBADF and
	// ECONNABORTED.
	if err := waitForHostinetSRCountAfter(logPath, lastCount+2); err != nil {
		t.Fatalf("wait for resumed progress: %v", err)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("error reading log file: %v", err)
	}
	if strings.Contains(string(b), "ERRNO=9\n") {
		t.Errorf("socket returned EBADF after checkpoint --leave-running:\n%s", b)
	}
	if strings.Contains(string(b), "ERRNO=103\n") {
		t.Errorf("socket returned ECONNABORTED after checkpoint --leave-running:\n%s", b)
	}
	select {
	case <-connClosed:
		t.Errorf("server connection closed after checkpoint --leave-running")
	default:
	}
}

// TestCheckpointHostinetRestoreNetworkMismatch checks that a checkpoint taken
// with host networking cannot be restored with sandbox networking.
func TestCheckpointHostinetRestoreNetworkMismatch(t *testing.T) {
	dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
	if err != nil {
		t.Fatalf("os.MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding file: %q, %v", dir, err)
	}

	conf := testutil.TestConfig(t)
	conf.Network = config.NetworkHost
	spec := testutil.NewSpecWithArgs("sleep", "1000")
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()

	args := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont.Destroy()
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}
	if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelFlateBestSpeed}); err != nil {
		t.Fatalf("error checkpointing container: %v", err)
	}

	restoreConf := *conf
	restoreConf.Network = config.NetworkSandbox
	args2 := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont2, err := New(&restoreConf, args2)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont2.Destroy()
	err = cont2.Restore(&restoreConf, dir, sandbox.RestoreOpts{})
	if err == nil {
		t.Fatalf("restore with mismatched network type succeeded, want error")
	}
	if !strings.Contains(err.Error(), "cannot be restored with") {
		t.Errorf("restore failed with %v, want network mismatch error", err)
	}
	loaded, err := Load(conf.RootDir, FullID{ContainerID: cont2.ID}, LoadOpts{})
	if err != nil {
		t.Fatalf("error loading container: %v", err)
	}
	if loaded.Status != Stopped {
		t.Errorf("got status %v after failed restore, want %v", loaded.Status, Stopped)
	}
}

// TestCheckpointRestoreExecKilled checks that exec'd processes are killed
// after the container is restored.
func TestCheckpointRestoreExecKilled(t *testing.T) {
	spec, conf := sleepSpecConf(t)
	_, bundleDir, cu, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cu()

	// Create and start the container.
	args := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont.Destroy()
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}

	execArgs := &controlapi.ExecArgs{
		Filename: "/bin/sleep",
		Argv:     []string{"/bin/sleep", "10000"},
	}
	pid1, err := cont.Execute(conf, execArgs)
	if err != nil {
		t.Fatalf("error executing in container: %v", err)
	}

	// Test exec process with stdio FDs. FDs will not be present after restore and
	// should be ignored.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdioCleanup := cleanup.Make(func() {
		r.Close()
		w.Close()
	})
	defer stdioCleanup.Clean()

	fdMap := map[int]*os.File{0: r, 1: w, 2: w}
	execArgs.FilePayload = controlapi.NewFilePayload(fdMap, nil)
	pid2, err := cont.Execute(conf, execArgs)
	if err != nil {
		t.Fatalf("error executing in container: %v", err)
	}

	// Since both share the same process name, ensure that the exec'd process
	// has a different PID than the init process.
	if pid1 == 1 || pid2 == 1 {
		t.Fatalf("exec'd PID cannot be 1")
	}
	// Wait until the init process and exec'd processes are present.
	expectedPL := []*controlapi.Process{
		newProcessBuilder().Cmd("sleep").PID(1).Process(),
		newProcessBuilder().Cmd("sleep").PID(int32(pid1)).Process(),
		newProcessBuilder().Cmd("sleep").PID(int32(pid2)).Process(),
	}
	if err := waitForProcessList(cont, expectedPL); err != nil {
		t.Fatalf("Failed to kill exec'ed process, err: %v", err)
	}

	// Set the image path, which is where the checkpoint image will be saved.
	dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
	if err != nil {
		t.Fatalf("os.MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding file: %q, %v", dir, err)
	}

	// Checkpoint running container.
	if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelFlateBestSpeed}); err != nil {
		t.Fatalf("error checkpointing container: %v", err)
	}
	cont.Destroy()
	cont = nil
	stdioCleanup.Clean()

	cont2, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont2.Destroy()

	if err := cont2.Restore(conf, dir, sandbox.RestoreOpts{}); err != nil {
		t.Fatalf("error restoring container: %v", err)
	}

	// Check that only the init process is present and the exec'ed
	// processes were killed.
	expectedPL = []*controlapi.Process{
		newProcessBuilder().Cmd("sleep").PID(1).Process(),
	}
	if err := waitForProcessList(cont2, expectedPL); err != nil {
		t.Fatalf("Failed to kill exec'ed process, err: %v", err)
	}
}

// TestCheckpointRestoreCreateMountPoint tests that mountpoints created during
// container creation are re-created after checkpoint/restore.
func TestCheckpointRestoreCreateMountPoint(t *testing.T) {
	dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
	if err != nil {
		t.Fatalf("os.MkdirTemp() failed: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding file: %q, %v", dir, err)
	}

	spec, conf := sleepSpecConf(t)

	mountDest := filepath.Join(dir, "/foo-dir")
	spec.Mounts = append(spec.Mounts, specs.Mount{
		Destination: mountDest,
		Type:        "tmpfs",
	})

	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()

	// Create and start the container.
	args := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont.Destroy()
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}
	if err := waitForProcessCount(cont, 1); err != nil {
		t.Fatal(err)
	}

	// Check that mount point was created.
	if ws, err := execute(conf, cont, "/usr/bin/test", "-d", mountDest); err != nil {
		t.Fatal(err)
	} else if ws != 0 {
		t.Fatalf("directory was not re-created upon restore, ws: %v", ws)
	}

	// Checkpoint running container; save state into new file.
	if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelDefault}); err != nil {
		t.Fatalf("error checkpointing container to file: %v", err)
	}

	// Remove directory created by the container.
	if err := os.RemoveAll(mountDest); err != nil {
		t.Fatalf("error removing mount point directory: %v", err)
	}

	// Destroy the original container to restore it in place.
	cont.Destroy()
	cont = nil

	cont2, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont2.Destroy()

	if err := cont2.Restore(conf, dir, sandbox.RestoreOpts{}); err != nil {
		t.Fatalf("error restoring container: %v", err)
	}

	// Check that mount point was re-created after restore.
	if ws, err := execute(conf, cont2, "/usr/bin/test", "-d", mountDest); err != nil {
		t.Fatal(err)
	} else if ws != 0 {
		t.Fatalf("directory was not re-created upon restore, ws: %v", ws)
	}
}

// TestCheckpointRestoreInodeNumbers checks that files on a bind mount keep
// their device and inode numbers across checkpoint/restore.
func TestCheckpointRestoreInodeNumbers(t *testing.T) {
	dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
	if err != nil {
		t.Fatalf("os.MkdirTemp() failed: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding file: %q, %v", dir, err)
	}
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0777); err != nil {
		t.Fatalf("os.Mkdir() failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "file"), nil, 0666); err != nil {
		t.Fatalf("os.WriteFile() failed: %v", err)
	}
	dst := filepath.Join(dir, "mnt")
	file := filepath.Join(dst, "file")

	spec, conf := sleepSpecConf(t)
	// Keep the file open so that its dentry is checkpointed.
	spec.Process.Args = []string{"/bin/sh", "-c", "exec 3<" + file + "; exec sleep 1000"}
	spec.Mounts = append(spec.Mounts, specs.Mount{Destination: dst, Source: src, Type: "bind"})

	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()

	args := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont.Destroy()
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}
	if err := waitForProcessCount(cont, 1); err != nil {
		t.Fatal(err)
	}
	before, err := executeCombinedOutput(conf, cont, nil, "/usr/bin/stat", "-c", "%d %i", file)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}

	if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelDefault}); err != nil {
		t.Fatalf("error checkpointing container to file: %v", err)
	}

	// Destroy the original container to restore it in place.
	cont.Destroy()
	cont = nil

	cont2, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont2.Destroy()
	if err := cont2.Restore(conf, dir, sandbox.RestoreOpts{}); err != nil {
		t.Fatalf("error restoring container: %v", err)
	}

	after, err := executeCombinedOutput(conf, cont2, nil, "/usr/bin/stat", "-c", "%d %i", file)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("device and inode numbers changed across restore: before %q, after %q", before, after)
	}
}

// TestCheckpointRestoreEROFS does the checkpoint/restore test on each platform using
// an EROFS image as the rootfs.
func TestCheckpointRestoreEROFS(t *testing.T) {
	// Skip this test if mkfs.erofs or busybox are not available.
	skipIfNotAvailable(t, "mkfs.erofs", "busybox")

	for _, tc := range []struct{ name, options string }{
		{name: "flat"},
		{name: "chunk-blockmap", options: "--chunksize=8192 -E force-inode-blockmap"},
		{name: "chunk-indexes", options: "--chunksize=8192 -E force-chunk-indexes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testDir, err := os.MkdirTemp(testutil.TmpDir(), "erofs_checkpoint_restore_test_")
			if err != nil {
				t.Fatalf("os.MkdirTemp() failed: %v", err)
			}
			defer os.RemoveAll(testDir)

			rootfsDir, rootfsImage, err := createRootfsEROFS(testDir, tc.options)
			if err != nil {
				t.Fatalf("failed to create EROFS rootfs image: %v", err)
			}

			// Skip overlay because test requires writing to host file.
			for name, conf := range configs(t, true /* noOverlay */) {
				t.Run(name, func(t *testing.T) {
					testCheckpointRestore(t, conf, statefile.CompressionLevelDefault, func(script string) *specs.Spec {
						spec := testutil.NewSpecWithArgs("/busybox", "sh", "-c", script)
						spec.Root = &specs.Root{
							Path:     rootfsDir,
							Readonly: false,
						}
						if spec.Annotations == nil {
							spec.Annotations = make(map[string]string)
						}
						spec.Annotations[sentryapi.RootfsPrefix+"type"] = erofs.Name
						spec.Annotations[sentryapi.RootfsPrefix+"source"] = rootfsImage
						// EROFS does not support creating synthetic directories
						// yet, so let's add a writeable and savable overlay for
						// rootfs, which allows the sentry to create the mount
						// point for the bind mount of the temporary directory
						// shared between host and test container.
						spec.Annotations[sentryapi.RootfsPrefix+"overlay"] = config.MemoryOverlay.String()
						return spec
					})
				})
			}
		})
	}
}

func TestCheckpointResume(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
			if err != nil {
				t.Fatalf("os.MkdirTemp failed: %v", err)
			}
			defer os.RemoveAll(dir)
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatalf("error chmoding file: %q, %v", dir, err)
			}

			outputPath := filepath.Join(dir, "output")
			outputFile, err := createWriteableOutputFile(outputPath)
			if err != nil {
				t.Fatalf("error creating output file: %v", err)
			}
			defer outputFile.Close()

			script := fmt.Sprintf("i=0; while true; do echo $i >> %q; sleep 1; i=$((i+1)); done", outputPath)
			spec := testutil.NewSpecWithArgs("bash", "-c", script)
			_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
			if err != nil {
				t.Fatalf("error setting up container: %v", err)
			}
			defer cleanup()

			// Create and start the container.
			args := Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			}
			cont, err := New(conf, args)
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			if err := cont.Start(conf); err != nil {
				t.Fatalf("error starting container: %v", err)
			}

			// Wait until application has ran.
			if err := waitForFileNotEmpty(outputFile); err != nil {
				t.Fatalf("Failed to wait for output file: %v", err)
			}

			// Checkpoint running container; save state into new file.
			if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{Resume: true}); err != nil {
				t.Fatalf("error checkpointing container to empty file: %v", err)
			}

			if !cont.Sandbox.Checkpointed {
				t.Fatalf("sandbox returned wrong value for Sandbox.Checkpointed, got: false, want: true")
			}

			if cont.Sandbox.Restored {
				t.Fatalf("sandbox returned wrong value for Sandbox.Restored, got: true, want: false")
			}
			cont.Destroy()
		})
	}
}

// TestUsageDiskBackedOverlay checks that data in a disk backed overlay is not
// reported as memory usage, before or after a checkpoint and restore.
func TestUsageDiskBackedOverlay(t *testing.T) {
	if !testutil.IsCheckpointSupported() {
		t.Skip("Checkpoint not supported")
	}
	conf := overlayTestConfig(t)

	const size = 64 << 20
	file := filepath.Join(testutil.TmpDir(), "usage-overlay")
	done := file + ".done"
	script := fmt.Sprintf("dd if=/dev/urandom of=%q bs=1M count=%d && echo ok > %q; while true; do sleep 1; done", file, size>>20, done)
	spec := testutil.NewSpecWithArgs("bash", "-c", script)
	bundleDir := setupBundle(t, conf, spec)
	cont := startContainer(t, conf, spec, bundleDir, nil)
	waitForGuestFiles(t, conf, cont, done)

	checkUsage := func(cont *Container, when string) {
		t.Helper()
		m, err := cont.Sandbox.Usage(true /* Full */)
		if err != nil {
			t.Fatalf("error getting usage %s: %v", when, err)
		}
		if m.Tmpfs >= size {
			t.Errorf("overlay data reported as tmpfs memory %s: got Tmpfs = %d, want < %d", when, m.Tmpfs, size)
		}
	}
	checkUsage(cont, "before checkpoint")

	dir := makeTempDir(t, "checkpoint")
	if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{}); err != nil {
		t.Fatalf("error checkpointing container: %v", err)
	}
	cont2 := newContainer(t, conf, spec, bundleDir, func(args *Args) {
		args.CheckpointDirPath = dir
	})
	if err := cont2.Restore(conf, dir, sandbox.RestoreOpts{}); err != nil {
		t.Fatalf("error restoring container: %v", err)
	}
	waitForGuestFiles(t, conf, cont2, file)
	checkUsage(cont2, "after restore")
}

func TestSplitFSCheckpointRestore(t *testing.T) {
	// We only run this test if checkpoint/restore is supported.
	if !testutil.IsCheckpointSupported() {
		t.Skip("Checkpoint not supported")
	}

	// We only test with overlay enabled.
	conf := testutil.TestConfig(t)
	overlayDir, err := os.MkdirTemp(testutil.TmpDir(), "overlay-dir")
	if err != nil {
		t.Fatalf("failed to create overlay directory: %v", err)
	}
	defer os.RemoveAll(overlayDir)
	if err := os.Chmod(overlayDir, 0777); err != nil {
		t.Fatalf("error chmoding overlay directory: %v", err)
	}
	conf.Overlay2.Set("all:dir=" + overlayDir)

	for _, tc := range []struct {
		name       string
		splitPath  string
		wantPrefix string
	}{
		{
			name:       "DefaultFilePrefix",
			splitPath:  "all-tmpfs",
			wantPrefix: checkpointfiles.FSCheckpointDir,
		},
		{
			name:       "DirectoryPrefix",
			splitPath:  "custom/=all-tmpfs",
			wantPrefix: "custom/",
		},
		{
			name:       "ExplicitFilePrefix",
			splitPath:  "custom=all-tmpfs",
			wantPrefix: "custom",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp(testutil.TmpDir(), "split-checkpoint-test")
			if err != nil {
				t.Fatalf("os.MkdirTemp failed: %v", err)
			}
			defer os.RemoveAll(dir)
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatalf("error chmoding file: %q, %v", dir, err)
			}

			// The file we will write to is in TmpDir() which is bind-mounted and overlay.
			guestFile := filepath.Join(testutil.TmpDir(), "test_file_"+tc.name)
			script := "echo hello > '" + guestFile + "'; while true; do sleep 1; done"
			spec := testutil.NewSpecWithArgs("bash", "-c", script)

			_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
			if err != nil {
				t.Fatalf("error setting up container: %v", err)
			}
			defer cleanup()

			args := Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			}
			cont, err := New(conf, args)
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont.Destroy()
			if err := cont.Start(conf); err != nil {
				t.Fatalf("error starting container: %v", err)
			}

			// Wait for container to start and write the file.
			err = testutil.Poll(func() error {
				ws, err := execute(conf, cont, "/bin/bash", "-c", fmt.Sprintf("[ -s %q ]", guestFile))
				if err != nil {
					return err
				}
				if ws.ExitStatus() != 0 {
					return fmt.Errorf("bash -c '[ -s %q ]' returned %d", guestFile, ws.ExitStatus())
				}
				return nil
			}, 5*time.Second)
			if err != nil {
				t.Fatalf("Failed to wait for %q: %v", guestFile, err)
			}

			// Verify that the file does NOT exist on the host (because of overlay).
			// guestFile path in host is the same because TmpDir() is bind-mounted to same path.
			if _, err := os.Stat(guestFile); !os.IsNotExist(err) {
				t.Errorf("File leaked to host! It should be in overlay only. Path: %q", guestFile)
			}

			// Checkpoint running container with SplitFSCheckpointPaths.
			checkpointOpts := sandbox.CheckpointOpts{
				SplitFSCheckpointPaths: []string{tc.splitPath},
			}
			if err := cont.Checkpoint(conf, dir, checkpointOpts); err != nil {
				t.Fatalf("error checkpointing container: %v", err)
			}

			// Verify that the expected directory (if directory-prefixed) and files are created.
			checkFSCheckpointFiles(t, dir, tc.wantPrefix)

			// Restore into a new container with different ID.
			args2 := Args{
				ID:                testutil.RandomContainerID(),
				Spec:              spec,
				BundleDir:         bundleDir,
				CheckpointDirPath: dir,
			}
			cont2, err := New(conf, args2)
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont2.Destroy()

			if err := cont2.Restore(conf, dir, sandbox.RestoreOpts{}); err != nil {
				t.Fatalf("error restoring container: %v", err)
			}

			// Verify that the file exists and contains "hello" in the restored container.
			stdout, err := executeCombinedOutput(conf, cont2, nil, "/bin/cat", guestFile)
			if err != nil {
				t.Fatalf("failed to execute cat %q: %v", guestFile, err)
			}
			if got := strings.TrimSpace(string(stdout)); got != "hello" {
				t.Errorf("unexpected content of %q: got %q, want %q", guestFile, got, "hello")
			}

			// Verify again that host file still does not exist.
			if _, err := os.Stat(guestFile); !os.IsNotExist(err) {
				t.Errorf("File leaked to host after restore! Path: %q", guestFile)
			}
		})
	}
}

// allTmpfs selects all the sandbox internal filesystems for the split
// filesystem checkpoint.
const allTmpfs = "all-tmpfs"

// fsCheckpointFiles are the base filenames that make up the filesystem part of
// a split checkpoint image.
var fsCheckpointFiles = []string{
	checkpointfiles.FSCheckpointManifestFileName,
	checkpointfiles.FSCheckpointMultiTarFileName,
	checkpointfiles.PagesFileName,
	checkpointfiles.PagesMetadataFileName,
}

func prefixedFSCheckpointFiles(prefix string) []string {
	var res []string
	for _, name := range fsCheckpointFiles {
		res = append(res, checkpointfiles.PrefixFileName(prefix, name))
	}
	return res
}

// sentryCheckpointFiles are the files that make up the Sentry part of a
// checkpoint image. They are saved at the root of the checkpoint directory.
var sentryCheckpointFiles = []string{
	checkpointfiles.StateFileName,
	checkpointfiles.PagesFileName,
	checkpointfiles.PagesMetadataFileName,
}

// makeTempDir creates a temporary directory that is removed when the test
// finishes. The directory is world-accessible because the sandboxed
// application may run as a different user than the test.
func makeTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp(testutil.TmpDir(), prefix)
	if err != nil {
		t.Fatalf("os.MkdirTemp(%q) failed: %v", prefix, err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding %q: %v", dir, err)
	}
	return dir
}

// overlayTestConfig returns a test config with overlay enabled for all gofer
// mounts, backed by a directory filestore. Split filesystem checkpoint only
// saves filesystems that are internal to the sandbox, so mounts must be
// overlaid for their content to be part of the checkpoint image.
func overlayTestConfig(t *testing.T) *config.Config {
	t.Helper()
	conf := testutil.TestConfig(t)
	conf.Overlay2.Set("all:dir=" + makeTempDir(t, "overlay-dir"))
	return conf
}

// addTmpfsMountToContainer adds a bind mount of source at dest to
// containerSpec, together with the mount hints on rootSpec that make the
// sandbox back the mount with a container private tmpfs. name identifies the
// mount in the annotations and must be unique within rootSpec.
func addTmpfsMountToContainer(rootSpec, containerSpec *specs.Spec, name, source, dest string) {
	containerSpec.Mounts = append(containerSpec.Mounts, specs.Mount{
		Destination: dest,
		Type:        "bind",
		Source:      source,
	})
	if rootSpec.Annotations == nil {
		rootSpec.Annotations = make(map[string]string)
	}
	prefix := "dev.gvisor.spec.mount." + name
	rootSpec.Annotations[prefix+".source"] = source
	rootSpec.Annotations[prefix+".type"] = "tmpfs"
	rootSpec.Annotations[prefix+".share"] = "container"
}

// addTmpfsMount adds a bind mount of source at dest to spec, together with the
// mount hints that make the sandbox back the mount with a container private
// tmpfs. name identifies the mount in the annotations and must be unique
// within the spec.
func addTmpfsMount(spec *specs.Spec, name, source, dest string) {
	addTmpfsMountToContainer(spec, spec, name, source, dest)
}

// setupBundle creates the root and bundle directories for spec and returns the
// bundle directory. Note that it also sets conf.RootDir, so containers created
// afterwards all share the same root directory.
func setupBundle(t *testing.T, conf *config.Config, spec *specs.Spec) string {
	t.Helper()
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	t.Cleanup(cleanup)
	return bundleDir
}

// newContainer creates a container for spec with a random ID. The container is
// destroyed when the test finishes. mutateArgs, if not nil, is called to adjust
// the container arguments before the container is created.
func newContainer(t *testing.T, conf *config.Config, spec *specs.Spec, bundleDir string, mutateArgs func(*Args)) *Container {
	t.Helper()
	args := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	if mutateArgs != nil {
		mutateArgs(&args)
	}
	cont, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	t.Cleanup(func() { cont.Destroy() })
	return cont
}

// startContainer creates and starts a container for spec. See newContainer.
func startContainer(t *testing.T, conf *config.Config, spec *specs.Spec, bundleDir string, mutateArgs func(*Args)) *Container {
	t.Helper()
	cont := newContainer(t, conf, spec, bundleDir, mutateArgs)
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}
	return cont
}

// waitForGuestFiles waits until all paths exist and are not empty inside the
// container.
func waitForGuestFiles(t *testing.T, conf *config.Config, cont *Container, paths ...string) {
	t.Helper()
	tests := make([]string, 0, len(paths))
	for _, path := range paths {
		tests = append(tests, fmt.Sprintf("[ -s %q ]", path))
	}
	script := strings.Join(tests, " && ")
	err := testutil.Poll(func() error {
		ws, err := execute(conf, cont, "/bin/bash", "-c", script)
		if err != nil {
			return err
		}
		if ws.ExitStatus() != 0 {
			return fmt.Errorf("bash -c %q returned %d", script, ws.ExitStatus())
		}
		return nil
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to wait for %v: %v", paths, err)
	}
}

// checkGuestFile checks that path inside the container contains want.
func checkGuestFile(t *testing.T, conf *config.Config, cont *Container, path, want string) {
	t.Helper()
	stdout, err := executeCombinedOutput(conf, cont, nil, "/bin/cat", path)
	if err != nil {
		t.Fatalf("failed to execute cat %q: %v", path, err)
	}
	if got := strings.TrimSpace(string(stdout)); got != want {
		t.Errorf("unexpected content of %q: got %q, want %q", path, got, want)
	}
}

// checkHostFilesAbsent checks that paths don't exist on the host. Files written
// to a filesystem that is internal to the sandbox, e.g. overlay or tmpfs, must
// never be visible to the host.
func checkHostFilesAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("file leaked to host, it should only exist inside the sandbox: %q", path)
		}
	}
}

// checkFSCheckpointFiles checks that the filesystem part of a split checkpoint
// image with the given prefix was created inside checkpointDir.
func checkFSCheckpointFiles(t *testing.T, checkpointDir, prefix string) {
	t.Helper()
	if strings.HasSuffix(prefix, "/") {
		fsDir := filepath.Join(checkpointDir, prefix)
		if _, err := os.Stat(fsDir); err != nil {
			t.Fatalf("filesystem checkpoint directory %q was not created: %v", fsDir, err)
		}
	}
	for _, name := range prefixedFSCheckpointFiles(prefix) {
		path := filepath.Join(checkpointDir, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %q was not created: %v", path, err)
		}
	}
}

// copyCheckpointFiles copies names from srcDir into a new temporary directory
// and returns it. Files that don't exist in srcDir are skipped. This is used to
// break a checkpoint image apart into its filesystem and Sentry parts.
func copyCheckpointFiles(t *testing.T, srcDir, dstPrefix string, names []string) string {
	t.Helper()
	dstDir := makeTempDir(t, dstPrefix)
	for _, name := range names {
		src := filepath.Join(srcDir, name)
		data, err := os.ReadFile(src)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("failed to read %q: %v", src, err)
		}
		dst := filepath.Join(dstDir, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatalf("failed to create directory %q: %v", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			t.Fatalf("failed to write %q: %v", dst, err)
		}
	}
	return dstDir
}

// TestPartialSplitFSCheckpointRestore tests that only the mounts listed in
// SplitFSCheckpointPaths are saved into the filesystem image, while the
// remaining ones are saved with the Sentry state:
//  1. Start a container with two tmpfs mounts and write a file to each of them.
//  2. Checkpoint it, passing only the first mount to the split filesystem
//     checkpoint. The second mount is saved as part of the Sentry state.
//  3. Restore into a new container and check that both files were restored,
//     i.e. that the two halves of the image are consistent with each other.
func TestPartialSplitFSCheckpointRestore(t *testing.T) {
	// We only run this test if checkpoint/restore is supported.
	if !testutil.IsCheckpointSupported() {
		t.Skip("Checkpoint not supported")
	}

	const tmpfsMount1 = "/tmpfs-split"
	const tmpfsMount2 = "/tmpfs-sentry"

	for _, tc := range []struct {
		name       string
		splitPath  string
		wantPrefix string
	}{
		{
			name:       "DirectoryPrefix",
			splitPath:  "fs/=" + tmpfsMount1,
			wantPrefix: checkpointfiles.FSCheckpointDir + "/",
		},
		{
			name:       "FilePrefix",
			splitPath:  tmpfsMount1,
			wantPrefix: checkpointfiles.FSCheckpointDir,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := overlayTestConfig(t)
			checkpointDir := makeTempDir(t, "partial-split-checkpoint-test")

			tmpfsSourceDir1 := makeTempDir(t, "tmpfs-source-1")
			tmpfsSourceDir2 := makeTempDir(t, "tmpfs-source-2")
			guestFile1 := filepath.Join(tmpfsMount1, "split_file")
			guestFile2 := filepath.Join(tmpfsMount2, "sentry_file")
			hostFile1 := filepath.Join(tmpfsSourceDir1, "split_file")
			hostFile2 := filepath.Join(tmpfsSourceDir2, "sentry_file")

			script := fmt.Sprintf("echo split_data > %q; echo sentry_data > %q; while true; do sleep 1; done", guestFile1, guestFile2)
			spec := testutil.NewSpecWithArgs("bash", "-c", script)
			addTmpfsMount(spec, "tmpfs1", tmpfsSourceDir1, tmpfsMount1)
			addTmpfsMount(spec, "tmpfs2", tmpfsSourceDir2, tmpfsMount2)
			bundleDir := setupBundle(t, conf, spec)

			cont := startContainer(t, conf, spec, bundleDir, nil)
			waitForGuestFiles(t, conf, cont, guestFile1, guestFile2)
			checkHostFilesAbsent(t, hostFile1, hostFile2)

			// Checkpoint the running container with only tmpfsMount1 saved into the
			// filesystem image.
			checkpointOpts := sandbox.CheckpointOpts{
				SplitFSCheckpointPaths: []string{tc.splitPath},
			}
			if err := cont.Checkpoint(conf, checkpointDir, checkpointOpts); err != nil {
				t.Fatalf("error checkpointing container: %v", err)
			}
			checkFSCheckpointFiles(t, checkpointDir, tc.wantPrefix)

			// Restore into a new container, relying on auto-detection of the split
			// filesystem image.
			cont2 := newContainer(t, conf, spec, bundleDir, func(args *Args) {
				args.CheckpointDirPath = checkpointDir
			})
			if err := cont2.Restore(conf, checkpointDir, sandbox.RestoreOpts{}); err != nil {
				t.Fatalf("error restoring container: %v", err)
			}

			checkGuestFile(t, conf, cont2, guestFile1, "split_data")
			checkGuestFile(t, conf, cont2, guestFile2, "sentry_data")
			checkHostFilesAbsent(t, hostFile1, hostFile2)
		})
	}
}

// TestSplitFSCheckpointRestoreOnlyFS tests that the filesystem image of a split
// checkpoint can be consumed independently from the Sentry image. It
// checkpoints a container with a tmpfs mount, copies the filesystem image into
// a directory of its own, and then exercises the ways of restoring it:
//   - StartFreshWithOnlyFS: a brand new container, running a different
//     application and no Sentry image, is started with the filesystem image. It
//     must see the files that the checkpointed container had written.
//   - RestoreWithIsolatedFS: a container is restored from a Sentry image that
//     doesn't contain the filesystem image, which is passed separately.
//   - RestoreWithExplicitPrefixPath: a container is restored by passing
//     <dir>/<prefix> explicitly to FSRestoreImagePath.
func TestSplitFSCheckpointRestoreOnlyFS(t *testing.T) {
	// We only run this test if checkpoint/restore is supported.
	if !testutil.IsCheckpointSupported() {
		t.Skip("Checkpoint not supported")
	}

	for _, tc := range []struct {
		name       string
		splitPath  string
		wantPrefix string
	}{
		{
			name:       "DirectoryPrefix",
			splitPath:  "fs/=" + allTmpfs,
			wantPrefix: checkpointfiles.FSCheckpointDir + "/",
		},
		{
			name:       "FilePrefix",
			splitPath:  allTmpfs,
			wantPrefix: checkpointfiles.FSCheckpointDir,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := overlayTestConfig(t)
			checkpointDir := makeTempDir(t, "split-checkpoint-test")

			// Create a host directory that is bind-mounted and then turned into a
			// tmpfs through mount hints.
			tmpfsSourceDir := makeTempDir(t, "tmpfs-source")
			const tmpfsMount = "/tmpfs-mount"
			guestFile := filepath.Join(tmpfsMount, "test_file")

			script := fmt.Sprintf("echo hello > %q; while true; do sleep 1; done", guestFile)
			spec := testutil.NewSpecWithArgs("bash", "-c", script)
			addTmpfsMount(spec, "test-tmpfs", tmpfsSourceDir, tmpfsMount)
			bundleDir := setupBundle(t, conf, spec)

			cont := startContainer(t, conf, spec, bundleDir, nil)
			waitForGuestFiles(t, conf, cont, guestFile)

			checkpointOpts := sandbox.CheckpointOpts{
				SplitFSCheckpointPaths: []string{tc.splitPath},
			}
			if err := cont.Checkpoint(conf, checkpointDir, checkpointOpts); err != nil {
				t.Fatalf("error checkpointing container: %v", err)
			}

			// Copy the filesystem image into a directory that holds nothing else.
			checkFSCheckpointFiles(t, checkpointDir, tc.wantPrefix)
			onlyFSDir := copyCheckpointFiles(t, checkpointDir, "only-fs-dir", prefixedFSCheckpointFiles(tc.wantPrefix))

			t.Run("StartFreshWithOnlyFS", func(t *testing.T) {
				// The fresh container runs a different application and is started from
				// scratch, i.e. no Sentry state is restored.
				specFresh := testutil.NewSpecWithArgs("sleep", "100")
				addTmpfsMount(specFresh, "test-tmpfs", tmpfsSourceDir, tmpfsMount)
				bundleDirFresh := setupBundle(t, conf, specFresh)

				contFresh := startContainer(t, conf, specFresh, bundleDirFresh, func(args *Args) {
					args.FSRestoreImagePaths = []string{onlyFSDir}
				})
				if err := contFresh.WaitFSRestore(); err != nil {
					t.Fatalf("error waiting for filesystem restore: %v", err)
				}

				// The file written by the checkpointed container must be visible in the
				// freshly started container.
				checkGuestFile(t, conf, contFresh, guestFile, "hello")
			})

			t.Run("RestoreWithIsolatedFS", func(t *testing.T) {
				// Copy the Sentry image into a directory that doesn't contain the
				// filesystem image, which is passed to the container separately.
				sentryOnlyDir := copyCheckpointFiles(t, checkpointDir, "sentry-only-dir", sentryCheckpointFiles)

				contRestore := newContainer(t, conf, spec, bundleDir, func(args *Args) {
					args.FSRestoreImagePaths = []string{onlyFSDir}
				})
				if err := contRestore.Restore(conf, sentryOnlyDir, sandbox.RestoreOpts{}); err != nil {
					t.Fatalf("error restoring container with isolated fs checkpoint: %v", err)
				}

				checkGuestFile(t, conf, contRestore, guestFile, "hello")
			})

			t.Run("RestoreWithExplicitPrefixPath", func(t *testing.T) {
				sentryOnlyDir := copyCheckpointFiles(t, checkpointDir, "sentry-only-dir-explicit", sentryCheckpointFiles)

				contRestore := newContainer(t, conf, spec, bundleDir, func(args *Args) {
					args.FSRestoreImagePaths = []string{filepath.Join(onlyFSDir, tc.wantPrefix)}
				})
				if err := contRestore.Restore(conf, sentryOnlyDir, sandbox.RestoreOpts{}); err != nil {
					t.Fatalf("error restoring container with explicit <dir>/<prefix> fs checkpoint: %v", err)
				}

				checkGuestFile(t, conf, contRestore, guestFile, "hello")
			})
		})
	}
}

// TestCheckpointRestoreTUN tests that a TUN device created inside a container
// is preserved across multiple checkpoint (with resume) and restore cycles
// along with its addresses and routes, and can be modified after each restore.
func TestCheckpointRestoreTUN(t *testing.T) {
	if !testutil.IsCheckpointSupported() {
		t.Skip("Checkpoint not supported")
	}

	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			spec, _ := sleepSpecConf(t)
			_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
			if err != nil {
				t.Fatalf("error setting up container: %v", err)
			}
			defer cleanup()

			args := Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			}
			cont, err := New(conf, args)
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont.Destroy()
			if err := cont.Start(conf); err != nil {
				t.Fatalf("error starting container: %v", err)
			}

			// Create a persistent TUN interface and configure IPv4 and IPv6
			// addresses before checkpointing.
			setupCmd := "ip tuntap add dev tun0 mode tun && ip addr add 10.0.0.1/24 dev tun0 && ip addr add fd00::1/64 dev tun0 && ip link set tun0 up && ip addr show dev tun0"
			out, err := executeCombinedOutput(conf, cont, nil, "/bin/sh", "-c", setupCmd)
			if err != nil {
				t.Fatalf("failed to set up tun0 before checkpoint: %v (output: %s)", err, out)
			}
			if !strings.Contains(string(out), "10.0.0.1/24") || !strings.Contains(string(out), "fd00::1/64") {
				t.Fatalf("unexpected tun0 addresses before checkpoint: %s", out)
			}

			checkpointDir := makeTempDir(t, "checkpoint-tun-test")
			if err := cont.Checkpoint(conf, checkpointDir, sandbox.CheckpointOpts{Resume: true}); err != nil {
				t.Fatalf("error checkpointing container: %v", err)
			}

			// Verify tun0 is still intact in the resumed original container.
			out, err = executeCombinedOutput(conf, cont, nil, "/bin/sh", "-c", "ip addr show dev tun0")
			if err != nil {
				t.Fatalf("failed to query tun0 after resume: %v (output: %s)", err, out)
			}
			if !strings.Contains(string(out), "10.0.0.1/24") || !strings.Contains(string(out), "fd00::1/64") {
				t.Fatalf("unexpected tun0 addresses after resume: %s", out)
			}
			cont.Destroy()

			args2 := Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			}
			cont2, err := New(conf, args2)
			if err != nil {
				t.Fatalf("error creating restored container: %v", err)
			}
			defer cont2.Destroy()

			if err := cont2.Restore(conf, checkpointDir, sandbox.RestoreOpts{}); err != nil {
				t.Fatalf("error restoring container: %v", err)
			}

			// Verify tun0 and its addresses are preserved after restore, and that
			// addresses can be added/removed and new TUN devices can be created.
			verifyCmd := "ip addr show dev tun0 && ip addr add 10.0.0.2/24 dev tun0 && ip addr del 10.0.0.1/24 dev tun0 && ip addr add fd00::2/64 dev tun0 && ip addr del fd00::1/64 dev tun0 && ip tuntap add dev tun1 mode tun && ip addr show dev tun0 && ip link show dev tun1"
			out, err = executeCombinedOutput(conf, cont2, nil, "/bin/sh", "-c", verifyCmd)
			if err != nil {
				t.Fatalf("failed to verify tun0 after restore: %v (output: %s)", err, out)
			}
			if !strings.Contains(string(out), "10.0.0.2/24") || !strings.Contains(string(out), "fd00::2/64") {
				t.Fatalf("expected updated addresses on tun0 after restore, got: %s", out)
			}

			// Checkpoint the restored container a second time with Resume: true.
			checkpointDir2 := makeTempDir(t, "checkpoint-tun-test-2")
			if err := cont2.Checkpoint(conf, checkpointDir2, sandbox.CheckpointOpts{Resume: true}); err != nil {
				t.Fatalf("error checkpointing restored container: %v", err)
			}

			// Verify tun0 and tun1 are still intact in cont2 after the second checkpoint+resume.
			out, err = executeCombinedOutput(conf, cont2, nil, "/bin/sh", "-c", "ip addr show dev tun0 && ip link show dev tun1")
			if err != nil {
				t.Fatalf("failed to query tun0/tun1 after second resume: %v (output: %s)", err, out)
			}
			if !strings.Contains(string(out), "10.0.0.2/24") || !strings.Contains(string(out), "fd00::2/64") {
				t.Fatalf("unexpected tun0 addresses after second resume: %s", out)
			}
			cont2.Destroy()

			args3 := Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			}
			cont3, err := New(conf, args3)
			if err != nil {
				t.Fatalf("error creating second restored container: %v", err)
			}
			defer cont3.Destroy()

			if err := cont3.Restore(conf, checkpointDir2, sandbox.RestoreOpts{}); err != nil {
				t.Fatalf("error restoring container a second time: %v", err)
			}

			// Verify tun0 and tun1 are preserved across the second restore and can be modified.
			verifyCmd2 := "ip addr show dev tun0 && ip link show dev tun1 && ip addr add 10.0.0.3/24 dev tun0 && ip addr del 10.0.0.2/24 dev tun0 && ip addr add fd00::3/64 dev tun0 && ip addr del fd00::2/64 dev tun0 && ip addr add 10.0.1.1/24 dev tun1 && ip link set tun1 up && ip addr show dev tun0 && ip addr show dev tun1"
			out, err = executeCombinedOutput(conf, cont3, nil, "/bin/sh", "-c", verifyCmd2)
			if err != nil {
				t.Fatalf("failed to verify tun0/tun1 after second restore: %v (output: %s)", err, out)
			}
			if !strings.Contains(string(out), "10.0.0.3/24") || !strings.Contains(string(out), "fd00::3/64") || !strings.Contains(string(out), "10.0.1.1/24") {
				t.Fatalf("expected updated addresses on tun0/tun1 after second restore, got: %s", out)
			}
		})
	}
}

// TestMultiBundleSplitFSCheckpointRestore tests saving and restoring multiple
// independent filesystem checkpoint bundles (e.g. rootfs=/ and fs=/dur-data),
// including combining bundles from different checkpoints on fresh container
// start. The workload runs either in the root container of its sandbox or in a
// subcontainer of a sandbox with no root container.
func TestMultiBundleSplitFSCheckpointRestore(t *testing.T) {
	if !testutil.IsCheckpointSupported() {
		t.Skip("Checkpoint not supported")
	}
	for _, noRootContainer := range []bool{false, true} {
		t.Run(fmt.Sprintf("noRootContainer=%t", noRootContainer), func(t *testing.T) {
			testMultiBundleSplitFSCheckpointRestore(t, noRootContainer)
		})
	}
}

func testMultiBundleSplitFSCheckpointRestore(t *testing.T, noRootContainer bool) {
	const (
		rootfsPrefix = "rootfs/"
		fsPrefix     = "bundles/fs"
		durMount     = "/dur-data"
		rootFile     = "/rootfs_marker"
	)

	conf := overlayTestConfig(t)
	setupTestRootDir(t, conf)
	fsSaveDir := makeTempDir(t, "multi-bundle-fssave")
	checkpointDir := makeTempDir(t, "multi-bundle-checkpoint")

	tmpfsSourceDir := makeTempDir(t, "dur-source")
	durFile := filepath.Join(durMount, "dur_marker")
	env := splitFSTestEnv{
		conf:            conf,
		noRootContainer: noRootContainer,
		mountSource:     tmpfsSourceDir,
		mountDest:       durMount,
	}

	script := fmt.Sprintf("echo root_val_1 > %q; echo dur_val_1 > %q; while true; do sleep 1; done", rootFile, durFile)
	appArgs := []string{"bash", "-c", script}
	sb, app := env.start(t, nil /* mutateArgs */, appArgs...)
	waitForGuestFiles(t, conf, app, rootFile, durFile)

	bundleSpecs := []string{
		rootfsPrefix + "=/",
		fsPrefix + "=" + durMount,
	}

	// Save an initial multi-bundle filesystem checkpoint while keeping the
	// container running.
	if err := sb.FSSave(conf, fsSaveDir, sandbox.FSSaveOpts{Paths: bundleSpecs}); err != nil {
		t.Fatalf("error saving filesystem checkpoint: %v", err)
	}
	checkFSCheckpointFiles(t, fsSaveDir, rootfsPrefix)
	checkFSCheckpointFiles(t, fsSaveDir, fsPrefix)

	// Mutate both filesystems before taking a full split checkpoint so the two
	// checkpoints have distinct states.
	mutateScript := fmt.Sprintf("echo root_val_2 > %q && echo dur_val_2 > %q", rootFile, durFile)
	if ws, err := execute(conf, app, "/bin/bash", "-c", mutateScript); err != nil || ws.ExitStatus() != 0 {
		t.Fatalf("failed to mutate files before second checkpoint: ws=%v, err=%v", ws, err)
	}

	// Checkpoint with two bundles: rootfs and fs.
	env.checkpoint(t, sb, checkpointDir, sandbox.CheckpointOpts{SplitFSCheckpointPaths: bundleSpecs})
	checkFSCheckpointFiles(t, checkpointDir, rootfsPrefix)
	checkFSCheckpointFiles(t, checkpointDir, fsPrefix)

	rootfsDir := copyCheckpointFiles(t, checkpointDir, "isolated-rootfs", prefixedFSCheckpointFiles(rootfsPrefix))
	durDir := copyCheckpointFiles(t, checkpointDir, "isolated-fs", append(prefixedFSCheckpointFiles(fsPrefix), sentryCheckpointFiles...))

	for _, rtc := range []struct {
		name          string
		restoreDir    string
		fsRestorePath []string
		mutateArgs    func(*Args)
	}{
		{
			name:       "AutoDiscoverBothBundles",
			restoreDir: checkpointDir,
			mutateArgs: func(args *Args) { args.CheckpointDirPath = checkpointDir },
		},
		{
			name:          "IsolatedPrefixPathsRestore",
			restoreDir:    durDir,
			fsRestorePath: []string{filepath.Join(rootfsDir, rootfsPrefix), filepath.Join(durDir, fsPrefix)},
		},
	} {
		t.Run(rtc.name, func(t *testing.T) {
			_, appRestore := env.restore(t, rtc.restoreDir, func(args *Args) {
				args.FSRestoreImagePaths = rtc.fsRestorePath
				if rtc.mutateArgs != nil {
					rtc.mutateArgs(args)
				}
			}, appArgs...)
			checkGuestFile(t, conf, appRestore, rootFile, "root_val_2")
			checkGuestFile(t, conf, appRestore, durFile, "dur_val_2")
		})
	}

	t.Run("StartFreshCombiningBundlesFromDifferentCheckpoints", func(t *testing.T) {
		_, appFresh := env.start(t, func(args *Args) {
			args.FSRestoreImagePaths = []string{
				filepath.Join(fsSaveDir, rootfsPrefix),
				filepath.Join(checkpointDir, fsPrefix),
			}
		}, "sleep", "100")
		if err := appFresh.WaitFSRestore(); err != nil {
			t.Fatalf("error waiting for filesystem restore: %v", err)
		}

		checkGuestFile(t, conf, appFresh, rootFile, "root_val_1")
		checkGuestFile(t, conf, appFresh, durFile, "dur_val_2")
	})
}

// splitFSTestEnv starts, checkpoints and restores the sandboxes of a split
// filesystem checkpoint test.
type splitFSTestEnv struct {
	conf            *config.Config
	noRootContainer bool
	mountSource     string
	mountDest       string
}

func (e *splitFSTestEnv) newSpecs(sbID string, args ...string) (sbSpec, appSpec *specs.Spec) {
	if !e.noRootContainer {
		spec := testutil.NewSpecWithArgs(args...)
		spec.Root.Readonly = false
		addTmpfsMount(spec, "dur-mount", e.mountSource, e.mountDest)
		return spec, spec
	}
	sbSpec = noRootContainerSandboxSpec()
	appSpec = noRootContainerSubSpec(sbID, args...)
	appSpec.Annotations[specutils.ContainerdContainerNameAnnotation] = "app"
	appSpec.Root.Readonly = false
	addTmpfsMountToContainer(sbSpec, appSpec, "dur-mount", e.mountSource, e.mountDest)
	return sbSpec, appSpec
}

func (e *splitFSTestEnv) sandboxArgs(sbID string, mutateArgs func(*Args)) func(*Args) {
	return func(args *Args) {
		args.ID = sbID
		args.NoRootContainer = e.noRootContainer
		if mutateArgs != nil {
			mutateArgs(args)
		}
	}
}

func (e *splitFSTestEnv) start(t *testing.T, mutateArgs func(*Args), args ...string) (sb, app *Container) {
	t.Helper()
	sbID := testutil.RandomContainerID()
	sbSpec, appSpec := e.newSpecs(sbID, args...)
	sb = startContainer(t, e.conf, sbSpec, newBundleDir(t, sbSpec), e.sandboxArgs(sbID, mutateArgs))
	if !e.noRootContainer {
		return sb, sb
	}
	return sb, startContainer(t, e.conf, appSpec, newBundleDir(t, appSpec), nil /* mutateArgs */)
}

func (e *splitFSTestEnv) restore(t *testing.T, imagePath string, mutateArgs func(*Args), args ...string) (sb, app *Container) {
	t.Helper()
	restoreOne := func(spec *specs.Spec, mutateArgs func(*Args)) *Container {
		t.Helper()
		c := newContainer(t, e.conf, spec, newBundleDir(t, spec), mutateArgs)
		if err := c.Restore(e.conf, imagePath, sandbox.RestoreOpts{}); err != nil {
			t.Fatalf("error restoring container: %v", err)
		}
		return c
	}
	sbID := testutil.RandomContainerID()
	sbSpec, appSpec := e.newSpecs(sbID, args...)
	sb = restoreOne(sbSpec, e.sandboxArgs(sbID, mutateArgs))
	if !e.noRootContainer {
		return sb, sb
	}
	app = restoreOne(appSpec, nil /* mutateArgs */)
	if err := sb.WaitRestore(); err != nil {
		t.Fatalf("error waiting for restore: %v", err)
	}
	return sb, app
}

func (e *splitFSTestEnv) checkpoint(t *testing.T, sb *Container, imagePath string, opts sandbox.CheckpointOpts) {
	t.Helper()
	if e.noRootContainer {
		checkpointNoRootContainer(t, e.conf, sb, imagePath, opts)
		return
	}
	if err := sb.Checkpoint(e.conf, imagePath, opts); err != nil {
		t.Fatalf("error checkpointing container: %v", err)
	}
}

func newBundleDir(t *testing.T, spec *specs.Spec) string {
	t.Helper()
	bundleDir, cleanupBundle, err := testutil.SetupBundleDir(spec)
	if err != nil {
		t.Fatalf("error setting up bundle: %v", err)
	}
	t.Cleanup(cleanupBundle)
	return bundleDir
}

func adjustSnapshotMetadata(dir string, updateFn func(map[string]string)) error {
	stateFilePath := filepath.Join(dir, checkpointfiles.StateFileName)
	raw, err := os.ReadFile(stateFilePath)
	if err != nil {
		return err
	}
	const magicLen = 8
	if len(raw) < magicLen+8 {
		return fmt.Errorf("statefile too small")
	}
	magic := raw[:magicLen]
	metaLen := binary.BigEndian.Uint64(raw[magicLen : magicLen+8])
	if uint64(len(raw)) < uint64(magicLen)+8+metaLen {
		return fmt.Errorf("statefile corrupt: metadata length exceeds file size")
	}
	metaBytes := raw[magicLen+8 : uint64(magicLen)+8+metaLen]
	payload := raw[uint64(magicLen)+8+metaLen:]

	var meta map[string]string
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return fmt.Errorf("unmarshaling metadata: %w", err)
	}
	updateFn(meta)

	newMetaBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshaling metadata: %w", err)
	}

	var buf bytes.Buffer
	buf.Write(magic)
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(newMetaBytes)))
	buf.Write(lenBuf[:])
	buf.Write(newMetaBytes)
	buf.Write(payload)

	return os.WriteFile(stateFilePath, buf.Bytes(), 0644)
}

type clockReading struct {
	vdso int64
	sys  int64
	diff int64
	tsc  uint64
}

func parseClockReadings(t *testing.T, data []byte) []clockReading {
	t.Helper()
	var readings []clockReading
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "OK:") {
			continue
		}
		var r clockReading
		// Format: "OK: vdso=%d sys=%d diff=%d tsc=%d"
		_, err := fmt.Sscanf(line, "OK: vdso=%d sys=%d diff=%d tsc=%d", &r.vdso, &r.sys, &r.diff, &r.tsc)
		if err != nil {
			t.Fatalf("failed to parse clock reading line %q: %v", line, err)
		}
		readings = append(readings, r)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("failed to scan clock readings: %v", err)
	}
	return readings
}

func assertMonotonicReadings(t *testing.T, readings []clockReading) {
	t.Helper()
	if len(readings) == 0 {
		t.Fatalf("expected clock readings, got none")
	}
	for i := 1; i < len(readings); i++ {
		prev := readings[i-1]
		curr := readings[i]
		if curr.vdso < prev.vdso {
			t.Fatalf("reading[%d] VDSO went backwards: prev=%d current=%d (delta=%d)", i, prev.vdso, curr.vdso, curr.vdso-prev.vdso)
		}
		if curr.sys < prev.sys {
			t.Fatalf("reading[%d] syscall time went backwards: prev=%d current=%d (delta=%d)", i, prev.sys, curr.sys, curr.sys-prev.sys)
		}
		if curr.tsc < prev.tsc {
			t.Fatalf("reading[%d] RDTSC went backwards: prev=%d current=%d (delta=%d)", i, prev.tsc, curr.tsc, int64(curr.tsc)-int64(prev.tsc))
		}
	}
}

func waitForFileLineCount(f *os.File, minLines int) error {
	return testutil.Poll(func() error {
		data, err := os.ReadFile(f.Name())
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		count := 0
		for _, l := range lines {
			if strings.HasPrefix(l, "OK:") {
				count++
			}
		}
		if count < minLines {
			return fmt.Errorf("file %s has %d OK lines, want at least %d", f.Name(), count, minLines)
		}
		return nil
	}, 10*time.Second)
}

func runAdjustedTSCTest(t *testing.T, conf *config.Config, count int, offsetCycles uint64) {
	t.Helper()
	dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-vdso-test")
	if err != nil {
		t.Fatalf("os.MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding dir: %q, %v", dir, err)
	}

	outputPath := filepath.Join(dir, "output")
	outputFile, err := createWriteableOutputFile(outputPath)
	if err != nil {
		t.Fatalf("error creating output file: %v", err)
	}
	defer outputFile.Close()

	app, err := testutil.FindFile("test/cmd/test_app/test_app")
	if err != nil {
		t.Fatalf("error finding test_app: %v", err)
	}

	appArgs := []string{
		"vdso-clock",
		fmt.Sprintf("--file=%s", outputPath),
		fmt.Sprintf("--count=%d", count),
	}

	spec := testutil.NewSpecWithArgs(append([]string{app}, appArgs...)...)
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()

	args := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont, err := New(conf, args)
	if err != nil {
		t.Fatalf("error creating container: %v", err)
	}
	defer cont.Destroy()
	if err := cont.Start(conf); err != nil {
		t.Fatalf("error starting container: %v", err)
	}

	// Wait until application has started and written at least one pre-checkpoint reading.
	if err := waitForFileLineCount(outputFile, 1); err != nil {
		t.Fatalf("failed to wait for output file: %v", err)
	}

	// Checkpoint running container (first save).
	dir1 := filepath.Join(dir, "ckpt1")
	if err := os.MkdirAll(dir1, 0777); err != nil {
		t.Fatalf("os.MkdirAll(dir1) failed: %v", err)
	}
	if err := cont.Checkpoint(conf, dir1, sandbox.CheckpointOpts{}); err != nil {
		t.Fatalf("error checkpointing container (first save): %v", err)
	}

	preData1, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("error reading pre-checkpoint output: %v", err)
	}
	preReadings1 := parseClockReadings(t, preData1)
	if len(preReadings1) == 0 {
		t.Fatalf("expected at least one pre-checkpoint reading")
	}
	lastPreReading1 := preReadings1[len(preReadings1)-1]

	// Adjust the snapshot metadata to simulate a snapshot from a host with a higher TSC.
	if offsetCycles > 0 {
		if err := adjustSnapshotMetadata(dir1, func(meta map[string]string) {
			cycleStr, ok := meta["gvisor_tsc_snapshot_cycle"]
			if !ok {
				t.Fatalf("snapshot missing gvisor_tsc_snapshot_cycle metadata")
			}
			cycle, err := strconv.ParseUint(cycleStr, 10, 64)
			if err != nil {
				t.Fatalf("failed to parse gvisor_tsc_snapshot_cycle %q: %v", cycleStr, err)
			}
			meta["gvisor_tsc_snapshot_cycle"] = strconv.FormatUint(cycle+offsetCycles, 10)
		}); err != nil {
			t.Fatalf("failed to adjust snapshot metadata: %v", err)
		}
	}

	// Restore into a second container (first restore).
	args2 := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont2, err := New(conf, args2)
	if err != nil {
		t.Fatalf("error creating second container: %v", err)
	}
	defer cont2.Destroy()

	if err := cont2.Restore(conf, dir1, sandbox.RestoreOpts{}); err != nil {
		t.Fatalf("error restoring second container: %v", err)
	}

	// Wait until the restored container has written at least one reading with the adjusted TSC.
	if err := waitForFileLineCount(outputFile, len(preReadings1)+1); err != nil {
		t.Fatalf("failed to wait for post-first-restore output: %v", err)
	}

	// Checkpoint the second container (second save) without manual metadata adjustment.
	// Because cont2 is already running with an adjusted guest TSC, its saved snapshot
	// metadata must record the guest TSC (including cont2's TSC offset) so that
	// restoring into cont3 preserves the offset rather than resetting to the raw host TSC.
	dir2 := filepath.Join(dir, "ckpt2")
	if err := os.MkdirAll(dir2, 0777); err != nil {
		t.Fatalf("os.MkdirAll(dir2) failed: %v", err)
	}
	if err := cont2.Checkpoint(conf, dir2, sandbox.CheckpointOpts{}); err != nil {
		t.Fatalf("error checkpointing second container (second save): %v", err)
	}

	preData2, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("error reading pre-second-checkpoint output: %v", err)
	}
	preReadings2 := parseClockReadings(t, preData2)
	if len(preReadings2) <= len(preReadings1) {
		t.Fatalf("expected readings after first restore, got total=%d pre1=%d", len(preReadings2), len(preReadings1))
	}
	lastPreReading2 := preReadings2[len(preReadings2)-1]

	// Restore into a third container (second restore).
	args3 := Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	}
	cont3, err := New(conf, args3)
	if err != nil {
		t.Fatalf("error creating third container: %v", err)
	}
	defer cont3.Destroy()

	if err := cont3.Restore(conf, dir2, sandbox.RestoreOpts{}); err != nil {
		t.Fatalf("error restoring third container: %v", err)
	}

	// Wait for third container to finish executing and verify exit status and output.
	ws, err := cont3.Wait()
	if err != nil {
		t.Fatalf("Wait failed: %v", err)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("error reading output: %v", err)
	}
	t.Logf("Container output:\n%s", string(data))

	if ws.ExitStatus() != 0 {
		t.Fatalf("Container failed with status %v: %s", ws, string(data))
	}
	if strings.Contains(string(data), "FAIL:") {
		t.Fatalf("Failure detected in output: %s", string(data))
	}

	readings := parseClockReadings(t, data)
	if len(readings) <= len(preReadings2) {
		t.Fatalf("expected post-second-restore readings, got none (total %d, pre2 %d)", len(readings), len(preReadings2))
	}
	assertMonotonicReadings(t, readings)

	if offsetCycles > 0 {
		firstPostReading1 := readings[len(preReadings1)]
		wantMinTSC1 := lastPreReading1.tsc + offsetCycles
		if firstPostReading1.tsc < wantMinTSC1 {
			t.Fatalf("RDTSC went backwards across first restore from host with higher TSC: pre+offset=%d, post=%d (delta=%d)",
				wantMinTSC1, firstPostReading1.tsc, int64(firstPostReading1.tsc)-int64(wantMinTSC1))
		}

		firstPostReading2 := readings[len(preReadings2)]
		if firstPostReading2.tsc < lastPreReading2.tsc {
			t.Fatalf("RDTSC went backwards across second restore: pre2=%d, post2=%d (delta=%d)",
				lastPreReading2.tsc, firstPostReading2.tsc, int64(firstPostReading2.tsc)-int64(lastPreReading2.tsc))
		}
	}
}

// TestCheckpointRestoreAdjustedTSC verifies that VDSO clock_gettime and syscall
// clock_gettime remain synchronized and that time does not go backwards when
// restoring a container snapshot with an adjusted TSC, including across chained
// save/restore cycles.
func TestCheckpointRestoreAdjustedTSC(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			if conf.Platform != "kvm" {
				t.Skip("TSC adjustment is only supported on KVM")
			}
			const offsetCycles = 10_000_000_000 // ~4s
			runAdjustedTSCTest(t, conf, 30 /* count */, offsetCycles)
		})
	}
}

type workloadHopSpec struct {
	saveEngine       config.StateEnginePolicy
	restoreEngine    config.StateEnginePolicy
	compression      statefile.CompressionLevel
	inspectStatefile bool
}

// workloadRootfsDir is the directory that runActiveWorkloadChain has the
// workload exercise in an overlay root filesystem. It must not exist on the
// host.
const workloadRootfsDir = "/gvisor-state-workload-rootfs"

// runActiveWorkloadChain runs the state-workload test app through the given
// checkpoint/restore hops. Besides a tmpfs mount and a host bind mount, the
// workload exercises the root filesystem if overlay (an --overlay2 value)
// overlays it.
func runActiveWorkloadChain(t *testing.T, conf *config.Config, overlay string, initEngine config.StateEnginePolicy, hops []workloadHopSpec) {
	t.Helper()
	rootOverlay := strings.HasPrefix(overlay, "root:")
	if rootOverlay {
		if _, err := os.Stat(workloadRootfsDir); !os.IsNotExist(err) {
			t.Fatalf("%s must not exist on the host: stat error %v", workloadRootfsDir, err)
		}
	}

	app, err := testutil.FindFile("test/cmd/test_app/test_app")
	if err != nil {
		t.Fatalf("error finding test_app: %v", err)
	}

	dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-active-workload")
	if err != nil {
		t.Fatalf("os.MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("error chmoding dir: %q, %v", dir, err)
	}

	controlDir := filepath.Join(dir, "control")
	if err := os.MkdirAll(controlDir, 0777); err != nil {
		t.Fatalf("os.MkdirAll(controlDir) failed: %v", err)
	}
	if err := os.Chmod(controlDir, 0777); err != nil {
		t.Fatalf("error chmoding controlDir: %v", err)
	}

	// hostDir is bind mounted into the container at hostMountPoint. The mount
	// point must already exist since the root filesystem is the host's.
	hostDir := filepath.Join(dir, "hostbind-src")
	hostMountPoint := filepath.Join(dir, "hostbind-mnt")
	for _, d := range []string{hostDir, hostMountPoint} {
		if err := os.MkdirAll(d, 0777); err != nil {
			t.Fatalf("os.MkdirAll(%q) failed: %v", d, err)
		}
		if err := os.Chmod(d, 0777); err != nil {
			t.Fatalf("error chmoding %q: %v", d, err)
		}
	}

	readWorkloadLog := func() string {
		data, err := os.ReadFile(filepath.Join(controlDir, "workload.log"))
		if err != nil {
			return fmt.Sprintf("<unable to read workload.log: %v>", err)
		}
		return string(data)
	}

	args := []string{
		app,
		"state-workload",
		"--control-dir=" + controlDir,
		"--scratch-dir=/scratch",
		"--host-dir=" + hostMountPoint,
		fmt.Sprintf("--hops=%d", len(hops)),
	}
	if rootOverlay {
		args = append(args, "--rootfs-dir="+workloadRootfsDir)
	}
	spec := testutil.NewSpecWithArgs(args...)
	// A read-only root is never overlaid; make it writable so that writes go
	// to the overlay's upper layer.
	spec.Root.Readonly = !rootOverlay
	spec.Mounts = append(spec.Mounts,
		specs.Mount{
			Destination: "/scratch",
			Type:        "tmpfs",
		},
		specs.Mount{
			Destination: hostMountPoint,
			Source:      hostDir,
			Type:        "bind",
		},
	)

	baseConf := *conf
	if err := baseConf.Overlay2.Set(overlay); err != nil {
		t.Fatalf("Overlay2.Set(%q): %v", overlay, err)
	}
	_, bundleDir, cleanup, err := testutil.SetupContainer(spec, &baseConf)
	if err != nil {
		t.Fatalf("error setting up container: %v", err)
	}
	defer cleanup()

	startConf := baseConf
	startConf.StateEngine = initEngine
	curCont, err := New(&startConf, Args{
		ID:        testutil.RandomContainerID(),
		Spec:      spec,
		BundleDir: bundleDir,
	})
	if err != nil {
		t.Fatalf("error creating initial container: %v", err)
	}
	defer func() {
		if curCont != nil {
			curCont.Destroy()
		}
	}()
	if err := curCont.Start(&startConf); err != nil {
		t.Fatalf("error starting initial container: %v", err)
	}

	for hopIdx, hop := range hops {
		readyPath := filepath.Join(controlDir, fmt.Sprintf("phase%d.ready", hopIdx))
		if err := testutil.Poll(func() error {
			if logData := readWorkloadLog(); strings.Contains(logData, "FAIL:") {
				return fmt.Errorf("workload reported failure:\n%s", logData)
			}
			_, err := os.Stat(readyPath)
			return err
		}, 30*time.Second); err != nil {
			t.Fatalf("hop %d timed out waiting for %s: %v\nworkload log:\n%s", hopIdx, readyPath, err, readWorkloadLog())
		}

		ckptDir := filepath.Join(dir, fmt.Sprintf("ckpt-%d", hopIdx))
		if err := os.MkdirAll(ckptDir, 0777); err != nil {
			t.Fatalf("hop %d MkdirAll(%s): %v", hopIdx, ckptDir, err)
		}
		if err := os.Chmod(ckptDir, 0777); err != nil {
			t.Fatalf("hop %d Chmod(%s): %v", hopIdx, ckptDir, err)
		}

		saveConf := baseConf
		saveConf.StateEngine = hop.saveEngine
		if err := curCont.Checkpoint(&saveConf, ckptDir, sandbox.CheckpointOpts{Compression: hop.compression}); err != nil {
			t.Fatalf("hop %d Checkpoint (engine=%s): %v\nworkload log:\n%s", hopIdx, hop.saveEngine, err, readWorkloadLog())
		}
		curCont.Destroy()
		curCont = nil

		imgPath := filepath.Join(ckptDir, checkpointfiles.StateFileName)
		if out, err := exec.Command(specutils.ExePath, "statefile", "-list", imgPath).CombinedOutput(); err != nil {
			t.Fatalf("hop %d statefile -list failed: %v\n%s", hopIdx, err, out)
		} else if !strings.Contains(string(out), sentryapi.CPUFeaturesKey) {
			t.Errorf("hop %d statefile -list output %q missing %q", hopIdx, out, sentryapi.CPUFeaturesKey)
		}

		if hop.inspectStatefile {
			dumpPath := filepath.Join(ckptDir, "statefile-dump.txt")
			if out, err := exec.Command(specutils.ExePath, "statefile", "-output="+dumpPath, imgPath).CombinedOutput(); err != nil {
				t.Fatalf("hop %d statefile dump failed: %v\n%s%s", hopIdx, err, out, preserveStatefile(t, hopIdx, imgPath))
			}
			dumpBytes, err := os.ReadFile(dumpPath)
			if err != nil {
				t.Fatalf("hop %d ReadFile(%s): %v", hopIdx, dumpPath, err)
			}
			dump := string(dumpBytes)
			wantRoot := "g1r0 = kernel.Kernel{"
			if hop.saveEngine == config.StateEngineWire {
				wantRoot = ".Kernel]{"
			}
			for _, wantSub := range []string{
				wantRoot,
				"EventFileDescription",
				"TimerFileDescription",
				"regularFileFD",
				"endpoint",
			} {
				if !strings.Contains(dump, wantSub) {
					t.Errorf("hop %d statefile dump (len=%d) missing expected substring %q", hopIdx, len(dump), wantSub)
				}
			}

			htmlPath := filepath.Join(ckptDir, "statefile-dump.html")
			if out, err := exec.Command(specutils.ExePath, "statefile", "-html", "-output="+htmlPath, imgPath).CombinedOutput(); err != nil {
				t.Fatalf("hop %d statefile -html dump failed: %v\n%s", hopIdx, err, out)
			}
		}

		restoreConf := baseConf
		restoreConf.StateEngine = hop.restoreEngine
		nextCont, err := New(&restoreConf, Args{
			ID:        testutil.RandomContainerID(),
			Spec:      spec,
			BundleDir: bundleDir,
		})
		if err != nil {
			t.Fatalf("hop %d error creating restored container: %v", hopIdx, err)
		}
		curCont = nextCont

		if err := curCont.Restore(&restoreConf, ckptDir, sandbox.RestoreOpts{}); err != nil {
			t.Fatalf("hop %d Restore (saveEngine=%s, restoreEngine=%s): %v\nworkload log:\n%s", hopIdx, hop.saveEngine, hop.restoreEngine, err, readWorkloadLog())
		}

		goPath := filepath.Join(controlDir, fmt.Sprintf("phase%d.go", hopIdx+1))
		if err := os.WriteFile(goPath, []byte("go\n"), 0666); err != nil {
			t.Fatalf("hop %d WriteFile(%s): %v", hopIdx, goPath, err)
		}
	}

	ws, err := curCont.Wait()
	if err != nil {
		t.Fatalf("Wait on final restored container failed: %v\nworkload log:\n%s", err, readWorkloadLog())
	}
	logOutput := readWorkloadLog()
	if ws.ExitStatus() != 0 {
		t.Fatalf("final restored container exited with status %v (exit=%d):\n%s", ws, ws.ExitStatus(), logOutput)
	}
	if _, err := os.Stat(filepath.Join(controlDir, "done.ok")); err != nil {
		t.Fatalf("missing done.ok marker after container exit: %v\nworkload log:\n%s", err, logOutput)
	}

	// Writes to the host bind mount, made both before and after each restore,
	// must have reached the host.
	probeDir := filepath.Join(hostDir, "fsprobe")
	var wantLog strings.Builder
	wantLog.WriteString("fsprobe:hostbind\n")
	for hop := range hops {
		fmt.Fprintf(&wantLog, "hop=%d\nverified=%d\n", hop, hop)
	}
	if got, err := os.ReadFile(filepath.Join(probeDir, "log")); err != nil || string(got) != wantLog.String() {
		t.Errorf("host bind mount log = (%q, %v), want %q", got, err, wantLog.String())
	}
	last := len(hops) - 1
	for _, name := range []string{
		fmt.Sprintf("renamed-%d", last),
		fmt.Sprintf("dir-%d/f", last),
		fmt.Sprintf("fresh-%d", last),
	} {
		if _, err := os.Stat(filepath.Join(probeDir, name)); err != nil {
			t.Errorf("host bind mount file %q missing on host: %v", name, err)
		}
	}
	if last > 0 {
		if _, err := os.Stat(filepath.Join(probeDir, fmt.Sprintf("renamed-%d", last-1))); !os.IsNotExist(err) {
			t.Errorf("host bind mount rename source still exists on host: stat error %v", err)
		}
	}
	// Writes to the overlay root must not have reached the host.
	if rootOverlay {
		if _, err := os.Stat(workloadRootfsDir); !os.IsNotExist(err) {
			t.Errorf("overlay root write leaked to host %s: stat error %v", workloadRootfsDir, err)
		}
	}
}

// preserveStatefile copies the statefile at imgPath to the test's undeclared
// outputs directory, if any, so that failures to inspect it can be debugged.
// It returns a message describing where the copy is, or why there is none.
func preserveStatefile(t *testing.T, hopIdx int, imgPath string) string {
	outDir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if outDir == "" {
		return ""
	}
	data, err := os.ReadFile(imgPath)
	if err != nil {
		return fmt.Sprintf("\n(failed to read statefile for preservation: %v)", err)
	}
	dst := filepath.Join(outDir, fmt.Sprintf("%s-hop%d-%s", strings.ReplaceAll(t.Name(), "/", "_"), hopIdx, checkpointfiles.StateFileName))
	if err := os.WriteFile(dst, data, 0644); err != nil {
		return fmt.Sprintf("\n(failed to preserve statefile: %v)", err)
	}
	return fmt.Sprintf("\nstatefile preserved as undeclared output %s", filepath.Base(dst))
}

// TestCheckpointRestoreActiveWorkload exercises chained multi-hop checkpoint
// and restore of a realistic multi-subsystem container workload (child worker
// process in separate PGID, shared memfd MAP_SHARED, private & file-backed
// mmaps, open/unlinked tmpfs files, seek offsets, advisory locks, xattrs,
// symlinks, custom signal handlers, loopback TCP/UDP with in-flight unread
// buffers, Unix sockets with in-flight SCM_RIGHTS FDs, and epoll over
// pipe/eventfd/timerfd), plus end-to-end `runsc statefile` graph formatting.
// Open, mapped, renamed, and unlinked files are exercised in a tmpfs mount, a
// host bind mount, and, with a root overlay, the root filesystem.
func TestCheckpointRestoreActiveWorkload(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		for _, overlay := range []string{"none", "root:memory"} {
			t.Run(name+"/overlay="+overlay, func(t *testing.T) {
				runActiveWorkloadChain(t, conf, overlay, config.StateEngineGraph, []workloadHopSpec{
					{
						saveEngine:    config.StateEngineGraph,
						restoreEngine: config.StateEngineGraph,
						compression:   statefile.CompressionLevelFlateBestSpeed,
					},
					{
						saveEngine:       config.StateEngineGraph,
						restoreEngine:    config.StateEngineGraph,
						compression:      statefile.CompressionLevelNone,
						inspectStatefile: true,
					},
				})
			})
		}
	}
}

// TestCheckpointRestoreCrossEngineChained verifies cross-engine compatibility
// across chained checkpoints (`wire -> graph -> wire` and `graph -> wire -> graph`)
// on a live multi-subsystem container workload, ensuring state.Load auto-detects
// stream format and restored Sentry object graphs re-serialize cleanly under
// either engine. The root filesystem is overlaid so that overlay state also
// crosses engines.
func TestCheckpointRestoreCrossEngineChained(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			t.Run("wire_to_graph_to_wire", func(t *testing.T) {
				runActiveWorkloadChain(t, conf, "root:memory", config.StateEngineWire, []workloadHopSpec{
					{
						saveEngine:       config.StateEngineWire,
						restoreEngine:    config.StateEngineGraph,
						compression:      statefile.CompressionLevelFlateBestSpeed,
						inspectStatefile: true,
					},
					{
						saveEngine:    config.StateEngineGraph,
						restoreEngine: config.StateEngineWire,
						compression:   statefile.CompressionLevelNone,
					},
				})
			})
			t.Run("graph_to_wire_to_graph", func(t *testing.T) {
				runActiveWorkloadChain(t, conf, "root:memory", config.StateEngineGraph, []workloadHopSpec{
					{
						saveEngine:    config.StateEngineGraph,
						restoreEngine: config.StateEngineWire,
						compression:   statefile.CompressionLevelNone,
					},
					{
						saveEngine:    config.StateEngineWire,
						restoreEngine: config.StateEngineGraph,
						compression:   statefile.CompressionLevelFlateBestSpeed,
					},
				})
			})
		})
	}
}

// TestCheckpointRestoreValidateFiles checks that a restore fails when a file
// that the sandbox had looked up changed since the checkpoint, in the
// filesystems that --restore-validate-files selects, as when a sandbox is
// restored onto another version of its image.
func TestCheckpointRestoreValidateFiles(t *testing.T) {
	app, err := testutil.FindFile("test/cmd/test_app/test_app")
	if err != nil {
		t.Fatalf("error finding test_app: %v", err)
	}
	for _, tc := range []struct {
		name   string
		policy string
		// change is the file changed after the checkpoint: the executable on
		// the root filesystem, the placeholder of a file mount on the root
		// filesystem, or an executable on a mount. Only the files that the
		// checkpoint holds, such as the executables of running processes,
		// are validated.
		change string
		// wantErr is a substring of the expected restore error, or empty if the
		// restore must succeed.
		wantErr string
	}{
		{
			name:    "rootfs, rootfs changed",
			policy:  "rootfs",
			change:  "rootfs/test_app",
			wantErr: "file size validation failed",
		},
		{
			name:   "none, rootfs changed",
			policy: "none",
			change: "rootfs/test_app",
		},
		{
			name:   "rootfs, mount point changed",
			policy: "rootfs",
			change: "rootfs/hosts",
		},
		{
			name:   "rootfs, mount changed",
			policy: "rootfs",
			change: "mnt/test_app",
		},
		{
			name:    "all, mount changed",
			policy:  "all",
			change:  "mnt/test_app",
			wantErr: "file size validation failed",
		},
	} {
		for name, conf := range configs(t, true /* noOverlay */) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				if err := conf.RestoreValidateFiles.Set(tc.policy); err != nil {
					t.Fatalf("error setting restore-validate-files: %v", err)
				}
				dir, err := os.MkdirTemp(testutil.TmpDir(), "validate-files")
				if err != nil {
					t.Fatalf("os.MkdirTemp() failed: %v", err)
				}
				defer os.RemoveAll(dir)
				rootfs := filepath.Join(dir, "rootfs")
				mnt := filepath.Join(dir, "mnt")
				imagePath := filepath.Join(dir, "image")
				for _, d := range []string{rootfs, filepath.Join(rootfs, "mnt"), mnt, imagePath} {
					if err := os.Mkdir(d, 0777); err != nil {
						t.Fatalf("os.Mkdir(%q) failed: %v", d, err)
					}
					if err := os.Chmod(d, 0777); err != nil {
						t.Fatalf("os.Chmod(%q) failed: %v", d, err)
					}
				}
				for _, d := range []string{rootfs, mnt} {
					if err := copyFile(app, filepath.Join(d, "test_app")); err != nil {
						t.Fatalf("error copying test_app: %v", err)
					}
				}
				// The mount point of a file mount, as a container manager
				// creates it for /etc/hosts.
				for _, f := range []string{filepath.Join(rootfs, "hosts"), filepath.Join(dir, "hosts")} {
					if err := os.WriteFile(f, nil, 0666); err != nil {
						t.Fatalf("os.WriteFile(%q) failed: %v", f, err)
					}
				}

				spec := testutil.NewSpecWithArgs("/test_app", "reaper")
				spec.Root = &specs.Root{Path: rootfs, Readonly: true}
				spec.Mounts = []specs.Mount{
					{
						Type:        "bind",
						Source:      mnt,
						Destination: "/mnt",
					},
					{
						Type:        "bind",
						Source:      filepath.Join(dir, "hosts"),
						Destination: "/hosts",
					},
				}
				_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
				if err != nil {
					t.Fatalf("error setting up container: %v", err)
				}
				defer cleanup()
				args := Args{
					ID:        testutil.RandomContainerID(),
					Spec:      spec,
					BundleDir: bundleDir,
				}
				cont, err := New(conf, args)
				if err != nil {
					t.Fatalf("error creating container: %v", err)
				}
				defer cont.Destroy()
				if err := cont.Start(conf); err != nil {
					t.Fatalf("error starting container: %v", err)
				}
				// Run a process from the mount, as the root process runs
				// from the root filesystem.
				if _, err := cont.Execute(conf, &controlapi.ExecArgs{Filename: "/mnt/test_app", Argv: []string{"/mnt/test_app", "reaper"}}); err != nil {
					t.Fatalf("error executing /mnt/test_app: %v", err)
				}

				if err := cont.Checkpoint(conf, imagePath, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelDefault}); err != nil {
					t.Fatalf("error checkpointing container: %v", err)
				}
				cont.Destroy()

				changed := filepath.Join(dir, tc.change)
				f, err := os.OpenFile(changed, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatalf("error opening %q: %v", changed, err)
				}
				_, err = f.Write([]byte("changed"))
				f.Close()
				if err != nil {
					t.Fatalf("error changing %q: %v", changed, err)
				}

				cont2, err := New(conf, args)
				if err != nil {
					t.Fatalf("error creating container: %v", err)
				}
				defer cont2.Destroy()
				err = cont2.Restore(conf, imagePath, sandbox.RestoreOpts{})
				switch {
				case tc.wantErr == "" && err != nil:
					t.Fatalf("restore after changing %q failed: %v", changed, err)
				case tc.wantErr != "" && err == nil:
					t.Fatalf("restore after changing %q succeeded, want an error containing %q", changed, tc.wantErr)
				case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
					t.Fatalf("restore after changing %q: got error %v, want it to contain %q", changed, err, tc.wantErr)
				}
			})
		}
	}
}

// TestCheckpointRestoreLeaveRunningClone checks that restoring the checkpoint
// of a container that kept running (runsc checkpoint --leave-running, or
// containerd's checkpoint without exit) into a new container gives a clone:
// both run, under their own container IDs, the clone from the state of the
// checkpoint. The application appends its host name, which the clone's spec
// changes, and a counter to a file.
func TestCheckpointRestoreLeaveRunningClone(t *testing.T) {
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
			if err != nil {
				t.Fatalf("os.MkdirTemp failed: %v", err)
			}
			defer os.RemoveAll(dir)
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatalf("error chmoding file: %q, %v", dir, err)
			}
			outputPath := filepath.Join(dir, "output")
			outputFile, err := createWriteableOutputFile(outputPath)
			if err != nil {
				t.Fatalf("error creating output file: %v", err)
			}
			defer outputFile.Close()
			imagePath := filepath.Join(dir, "image")
			if err := os.Mkdir(imagePath, 0777); err != nil {
				t.Fatalf("os.Mkdir failed: %v", err)
			}
			script := fmt.Sprintf(`i=0; while true; do echo "$(uname -n) $i" >> %q; i=$((i+1)); sleep 0.1; done`, outputPath)
			newContainer := func(hostname string) *Container {
				t.Helper()
				spec := testutil.NewSpecWithArgs("bash", "-c", script)
				spec.Hostname = hostname
				_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
				if err != nil {
					t.Fatalf("error setting up container: %v", err)
				}
				t.Cleanup(cleanup)
				cont, err := New(conf, Args{
					ID:        testutil.RandomContainerID(),
					Spec:      spec,
					BundleDir: bundleDir,
				})
				if err != nil {
					t.Fatalf("error creating container: %v", err)
				}
				t.Cleanup(func() { cont.Destroy() })
				return cont
			}
			// counters returns the counters that each host name appended, in
			// order.
			counters := func() map[string][]int {
				t.Helper()
				data, err := os.ReadFile(outputPath)
				if err != nil {
					t.Fatalf("error reading output: %v", err)
				}
				cs := make(map[string][]int)
				for _, line := range strings.Split(string(data), "\n") {
					hostname, counter, ok := strings.Cut(line, " ")
					if !ok {
						continue
					}
					i, err := strconv.Atoi(counter)
					if err != nil {
						t.Fatalf("invalid output line %q: %v", line, err)
					}
					cs[hostname] = append(cs[hostname], i)
				}
				return cs
			}

			original := newContainer("original")
			if err := original.Start(conf); err != nil {
				t.Fatalf("error starting container: %v", err)
			}
			if err := waitForFileNotEmpty(outputFile); err != nil {
				t.Fatalf("Failed to wait for output file: %v", err)
			}
			before := counters()["original"]
			if err := original.Checkpoint(conf, imagePath, sandbox.CheckpointOpts{Resume: true}); err != nil {
				t.Fatalf("error checkpointing container: %v", err)
			}

			clone := newContainer("clone")
			if clone.ID == original.ID {
				t.Fatalf("clone has the original's container ID %q", clone.ID)
			}
			if err := clone.Restore(conf, imagePath, sandbox.RestoreOpts{}); err != nil {
				t.Fatalf("error restoring container: %v", err)
			}
			restored := len(counters()["original"])

			// Both append at least 5 more lines.
			if err := testutil.Poll(func() error {
				cs := counters()
				if n := len(cs["original"]) - restored; n < 5 {
					return fmt.Errorf("original appended %d lines since the restore", n)
				}
				if n := len(cs["clone"]); n < 5 {
					return fmt.Errorf("clone appended %d lines", n)
				}
				return nil
			}, 30*time.Second); err != nil {
				t.Fatalf("both containers did not run: %v", err)
			}
			for _, cont := range []*Container{original, clone} {
				if running, err := cont.IsSandboxRunning(); err != nil || !running {
					t.Errorf("sandbox of container %q is not running (err: %v)", cont.ID, err)
				}
			}
			// The clone continues from the checkpoint, after the counters that
			// the original had appended before it.
			if first, last := counters()["clone"][0], before[len(before)-1]; first <= last {
				t.Errorf("clone's first counter is %d, want more than the original's last before the checkpoint, %d", first, last)
			}
		})
	}
}

// TestCheckpointRestorePaused checks that a container restored paused is
// Paused and runs nothing until it is resumed, and that the save/restore exec
// hook runs at its resume, in its restore mode.
func TestCheckpointRestorePaused(t *testing.T) {
	// Skip overlay because test requires writing to host file.
	for name, conf := range configs(t, true /* noOverlay */) {
		t.Run(name, func(t *testing.T) {
			dir, err := os.MkdirTemp(testutil.TmpDir(), "checkpoint-test")
			if err != nil {
				t.Fatalf("os.MkdirTemp failed: %v", err)
			}
			defer os.RemoveAll(dir)
			if err := os.Chmod(dir, 0777); err != nil {
				t.Fatalf("error chmoding file: %q, %v", dir, err)
			}

			outputPath := filepath.Join(dir, "output")
			outputFile, err := createWriteableOutputFile(outputPath)
			if err != nil {
				t.Fatalf("error creating output file: %v", err)
			}
			defer outputFile.Close()

			// The hook logs the mode it runs in.
			hookLogPath := filepath.Join(dir, "hook-log")
			hookLog, err := createWriteableOutputFile(hookLogPath)
			if err != nil {
				t.Fatalf("error creating hook log: %v", err)
			}
			hookLog.Close()
			hookPath := filepath.Join(dir, "hook.sh")
			if err := os.WriteFile(hookPath, []byte(fmt.Sprintf("echo $GVISOR_SAVE_RESTORE_AUTO_EXEC_MODE >> %q\n", hookLogPath)), 0755); err != nil {
				t.Fatalf("error writing hook: %v", err)
			}
			checkHookLog := func(want string) {
				t.Helper()
				got, err := os.ReadFile(hookLogPath)
				if err != nil {
					t.Fatalf("error reading hook log: %v", err)
				}
				if string(got) != want {
					t.Errorf("hook log: got %q, want %q", got, want)
				}
			}

			script := fmt.Sprintf("i=0; while true; do echo $i >> %q; sleep 0.1; i=$((i+1)); done", outputPath)
			spec := testutil.NewSpecWithArgs("bash", "-c", script)
			_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
			if err != nil {
				t.Fatalf("error setting up container: %v", err)
			}
			defer cleanup()

			cont, err := New(conf, Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			})
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont.Destroy()
			if err := cont.Start(conf); err != nil {
				t.Fatalf("error starting container: %v", err)
			}
			if err := waitForFileNotEmpty(outputFile); err != nil {
				t.Fatalf("Failed to wait for output file: %v", err)
			}

			if err := cont.Checkpoint(conf, dir, sandbox.CheckpointOpts{
				SaveRestoreExecArgv:        "/bin/bash " + hookPath,
				SaveRestoreExecTimeout:     controlapi.DefaultSaveRestoreExecTimeout,
				SaveRestoreExecContainerID: cont.ID,
			}); err != nil {
				t.Fatalf("error checkpointing container: %v", err)
			}
			checkHookLog("save\n")
			lastNum, err := readOutputNum(outputPath, -1)
			if err != nil {
				t.Fatalf("error with outputFile: %v", err)
			}
			cont.Destroy()

			// Delete and recreate file before restoring.
			if err := os.Remove(outputPath); err != nil {
				t.Fatalf("error removing file")
			}
			outputFile2, err := createWriteableOutputFile(outputPath)
			if err != nil {
				t.Fatalf("error creating output file: %v", err)
			}
			defer outputFile2.Close()

			cont2, err := New(conf, Args{
				ID:        testutil.RandomContainerID(),
				Spec:      spec,
				BundleDir: bundleDir,
			})
			if err != nil {
				t.Fatalf("error creating container: %v", err)
			}
			defer cont2.Destroy()
			if err := cont2.Restore(conf, dir, sandbox.RestoreOpts{Paused: true}); err != nil {
				t.Fatalf("error restoring container: %v", err)
			}
			if got, want := cont2.State().Status, Paused; got != want {
				t.Fatalf("restored container status: got %v, want %v", got, want)
			}

			// The application writes a number every 100ms when it runs.
			time.Sleep(time.Second)
			if fi, err := outputFile2.Stat(); err != nil {
				t.Fatalf("error with outputFile: %v", err)
			} else if fi.Size() != 0 {
				t.Fatalf("container restored paused ran: it wrote %d bytes", fi.Size())
			}
			checkHookLog("save\n")

			if err := cont2.Resume(); err != nil {
				t.Fatalf("error resuming container: %v", err)
			}
			if got, want := cont2.State().Status, Running; got != want {
				t.Errorf("resumed container status: got %v, want %v", got, want)
			}
			// Resume returns once the hook has run.
			checkHookLog("save\nrestore\n")
			firstNum, err := readOutputNum(outputPath, 0)
			if err != nil {
				t.Fatalf("error with outputFile: %v", err)
			}
			if lastNum+1 != firstNum {
				t.Errorf("error numbers not in order, previous: %d, next: %d", lastNum, firstNum)
			}
		})
	}
}

// TestCheckpointLeaveRunningRestoresWhatMovedOn checks what the restore of a
// checkpoint taken with --leave-running asks of the files that the container,
// running on, changes after it. The checkpoint holds only the files pinned at
// the save: an open file, and a mapped one, as a running executable is, and
// their directories. They must be as they were, and a restore fails, stopping
// its sandbox and gofer so that waiting on it returns, where one is gone or
// changed. A file the container only looked up is not held: the save evicts
// it from the dentry cache, and the container that runs on after the save,
// and the restored one, look it up as it is on the host now.
func TestCheckpointLeaveRunningRestoresWhatMovedOn(t *testing.T) {
	app, err := testutil.FindFile("test/cmd/test_app/test_app")
	if err != nil {
		t.Fatalf("error finding test_app: %v", err)
	}
	for _, tc := range []struct {
		name string
		// change changes the files of dir after the checkpoint.
		change func(dir string) error
		// looked is what the containers read of the looked-up file, and
		// entries how many entries they list in the looked-up directory,
		// once the change is made, where the restore must succeed.
		looked  string
		entries string
		// wantErr is a substring of the restore's error, or empty if it must
		// succeed.
		wantErr string
	}{
		{
			name:    "looked-up file deleted",
			change:  func(dir string) error { return os.Remove(filepath.Join(dir, "looked")) },
			looked:  "gone",
			entries: "2",
		},
		{
			name: "looked-up file changed",
			change: func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "looked"), []byte("changed since"), 0666)
			},
			looked:  "changed since",
			entries: "2",
		},
		{
			name:    "looked-up directory deleted",
			change:  func(dir string) error { return os.RemoveAll(filepath.Join(dir, "dir")) },
			looked:  "before",
			entries: "0",
		},
		{
			name:    "open file deleted",
			change:  func(dir string) error { return os.Remove(filepath.Join(dir, "open")) },
			wantErr: "failed to walk",
		},
		{
			name:    "open file changed",
			change:  func(dir string) error { return appendTo(filepath.Join(dir, "open")) },
			wantErr: "file size validation failed",
		},
		{
			name:    "mapped file deleted",
			change:  func(dir string) error { return os.Remove(filepath.Join(dir, "test_app")) },
			wantErr: "failed to walk",
		},
		{
			name:    "mapped file changed",
			change:  func(dir string) error { return appendTo(filepath.Join(dir, "test_app")) },
			wantErr: "file size validation failed",
		},
	} {
		for name, conf := range configs(t, true /* noOverlay */) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				if err := conf.RestoreValidateFiles.Set("all"); err != nil {
					t.Fatalf("error setting restore-validate-files: %v", err)
				}
				dir, err := os.MkdirTemp(testutil.TmpDir(), "moved-on")
				if err != nil {
					t.Fatalf("os.MkdirTemp failed: %v", err)
				}
				defer os.RemoveAll(dir)
				imagePath := filepath.Join(dir, "image")
				for _, d := range []string{dir, imagePath, filepath.Join(dir, "dir")} {
					if err := os.MkdirAll(d, 0777); err != nil {
						t.Fatalf("os.MkdirAll(%q) failed: %v", d, err)
					}
					if err := os.Chmod(d, 0777); err != nil {
						t.Fatalf("os.Chmod(%q) failed: %v", d, err)
					}
				}
				for path, content := range map[string]string{"looked": "before", "dir/a": "a", "dir/b": "b", "open": "open"} {
					if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0666); err != nil {
						t.Fatalf("os.WriteFile(%q) failed: %v", path, err)
					}
				}
				if err := copyFile(app, filepath.Join(dir, "test_app")); err != nil {
					t.Fatalf("error copying test_app: %v", err)
				}
				outputPath := filepath.Join(dir, "output")
				// The shell holds open, and test_app maps, a file of dir, and each
				// round looks up the others and says what it found.
				script := fmt.Sprintf(`exec 3<%[1]s/open
%[1]s/test_app reaper &
while true; do
  looked=$(cat %[1]s/looked 2>/dev/null || echo gone)
  cat %[1]s/dir/a > /dev/null 2>&1
  echo "$(uname -n) $(ls %[1]s/dir 2>/dev/null | wc -l) $looked" >> %[2]s
  sleep 0.1
done`, dir, outputPath)
				newContainer := func(hostname string) *Container {
					t.Helper()
					spec := testutil.NewSpecWithArgs("bash", "-c", script)
					spec.Hostname = hostname
					_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
					if err != nil {
						t.Fatalf("error setting up container: %v", err)
					}
					t.Cleanup(cleanup)
					cont, err := New(conf, Args{ID: testutil.RandomContainerID(), Spec: spec, BundleDir: bundleDir})
					if err != nil {
						t.Fatalf("error creating container: %v", err)
					}
					t.Cleanup(func() { cont.Destroy() })
					return cont
				}
				// lastSaid is what the container of hostname said last, and
				// how many times it said anything.
				lastSaid := func(hostname string) (string, int) {
					t.Helper()
					data, err := os.ReadFile(outputPath)
					if err != nil && !os.IsNotExist(err) {
						t.Fatalf("error reading output: %v", err)
					}
					last, n := "", 0
					for _, line := range strings.Split(string(data), "\n") {
						if said, ok := strings.CutPrefix(line, hostname+" "); ok {
							last, n = said, n+1
						}
					}
					return last, n
				}
				says := func(hostname, want string) error {
					return testutil.Poll(func() error {
						if got, _ := lastSaid(hostname); got != want {
							return fmt.Errorf("%s says %q, want %q", hostname, got, want)
						}
						return nil
					}, 30*time.Second)
				}

				original := newContainer("original")
				if err := original.Start(conf); err != nil {
					t.Fatalf("error starting container: %v", err)
				}
				if err := says("original", "2 before"); err != nil {
					t.Fatal(err)
				}
				if err := original.Checkpoint(conf, imagePath, sandbox.CheckpointOpts{Resume: true}); err != nil {
					t.Fatalf("error checkpointing container: %v", err)
				}
				if err := tc.change(dir); err != nil {
					t.Fatalf("error changing the files: %v", err)
				}
				// The original runs on after the save that evicted its cache,
				// and finds its files as they are now.
				if tc.wantErr == "" {
					if err := says("original", tc.entries+" "+tc.looked); err != nil {
						t.Fatalf("after the save: %v", err)
					}
				} else {
					_, before := lastSaid("original")
					if err := testutil.Poll(func() error {
						if _, n := lastSaid("original"); n < before+5 {
							return fmt.Errorf("original said %d more lines", n-before)
						}
						return nil
					}, 30*time.Second); err != nil {
						t.Fatalf("after the save: %v", err)
					}
				}

				clone := newContainer("clone")
				err = clone.Restore(conf, imagePath, sandbox.RestoreOpts{})
				if tc.wantErr == "" {
					if err != nil {
						t.Fatalf("restore failed: %v", err)
					}
					if err := says("clone", tc.entries+" "+tc.looked); err != nil {
						t.Fatalf("after the restore: %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("restore: got error %v, want one containing %q", err, tc.wantErr)
				}
				// The failed restore left nothing running, and waiting on it
				// returns.
				if running, err := clone.IsSandboxRunning(); err != nil || running {
					t.Errorf("the clone's sandbox runs (%v) after its restore failed: %v", running, err)
				}
				// The gofer, killed, is a child of this process here, a zombie
				// until clone.Wait reaps it: it is gone, or one, once it ended.
				if goferPid := clone.GoferPid.Load(); goferPid != 0 {
					if err := testutil.Poll(func() error {
						stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", goferPid))
						if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
							return nil
						}
						if err != nil {
							return &backoff.PermanentError{Err: err}
						}
						// Its state is the first field past its name's ")".
						fields := strings.Fields(string(stat[strings.LastIndex(string(stat), ")")+1:]))
						if len(fields) == 0 || fields[0] != "Z" {
							return fmt.Errorf("its state is %q", fields)
						}
						return nil
					}, 10*time.Second); err != nil {
						t.Errorf("the clone's gofer %d lives after its restore failed: %v", goferPid, err)
					}
				}
				if clone.Status != Stopped {
					t.Errorf("the clone is %v after its restore failed, want %v", clone.Status, Stopped)
				}
				waited := make(chan error, 1)
				go func() {
					_, err := clone.Wait()
					waited <- err
				}()
				select {
				case <-waited:
				case <-time.After(30 * time.Second):
					t.Fatal("waiting on the clone did not return after its restore failed")
				}
				if running, err := original.IsSandboxRunning(); err != nil || !running {
					t.Errorf("the original's sandbox stopped (err: %v)", err)
				}
			})
		}
	}
}

// appendTo appends to the file at path, which changes its size.
func appendTo(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, err = f.Write([]byte("changed"))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// TestCheckpointRestorePausedKilled checks that a container restored paused,
// never resumed, is killed with runsc kill --all and destroyed, each within
// 10 s, as the shim does as its pod is deleted, its sandbox then gone: whether
// its restore succeeded, or failed, the root filesystem's executable having
// changed since the checkpoint.
func TestCheckpointRestorePausedKilled(t *testing.T) {
	app, err := testutil.FindFile("test/cmd/test_app/test_app")
	if err != nil {
		t.Fatalf("error finding test_app: %v", err)
	}
	for _, tc := range []struct {
		name   string
		failed bool
	}{
		{name: "restored"},
		{name: "restore failed", failed: true},
	} {
		for name, conf := range configs(t, true /* noOverlay */) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				if err := conf.RestoreValidateFiles.Set("rootfs"); err != nil {
					t.Fatalf("error setting restore-validate-files: %v", err)
				}
				dir, err := os.MkdirTemp(testutil.TmpDir(), "paused-killed")
				if err != nil {
					t.Fatalf("os.MkdirTemp() failed: %v", err)
				}
				defer os.RemoveAll(dir)
				rootfs := filepath.Join(dir, "rootfs")
				imagePath := filepath.Join(dir, "image")
				for _, d := range []string{rootfs, imagePath} {
					if err := os.Mkdir(d, 0777); err != nil {
						t.Fatalf("os.Mkdir(%q) failed: %v", d, err)
					}
					if err := os.Chmod(d, 0777); err != nil {
						t.Fatalf("os.Chmod(%q) failed: %v", d, err)
					}
				}
				if err := copyFile(app, filepath.Join(rootfs, "test_app")); err != nil {
					t.Fatalf("error copying test_app: %v", err)
				}
				spec := testutil.NewSpecWithArgs("/test_app", "reaper")
				spec.Root = &specs.Root{Path: rootfs, Readonly: true}
				_, bundleDir, cleanup, err := testutil.SetupContainer(spec, conf)
				if err != nil {
					t.Fatalf("error setting up container: %v", err)
				}
				defer cleanup()
				args := Args{
					ID:        testutil.RandomContainerID(),
					Spec:      spec,
					BundleDir: bundleDir,
				}
				cont, err := New(conf, args)
				if err != nil {
					t.Fatalf("error creating container: %v", err)
				}
				defer cont.Destroy()
				if err := cont.Start(conf); err != nil {
					t.Fatalf("error starting container: %v", err)
				}
				if err := cont.Checkpoint(conf, imagePath, sandbox.CheckpointOpts{Compression: statefile.CompressionLevelDefault}); err != nil {
					t.Fatalf("error checkpointing container: %v", err)
				}
				cont.Destroy()

				if tc.failed {
					changed := filepath.Join(rootfs, "test_app")
					f, err := os.OpenFile(changed, os.O_WRONLY|os.O_APPEND, 0)
					if err != nil {
						t.Fatalf("error opening %q: %v", changed, err)
					}
					_, err = f.Write([]byte("changed"))
					f.Close()
					if err != nil {
						t.Fatalf("error changing %q: %v", changed, err)
					}
				}

				args.ID = testutil.RandomContainerID()
				cont2, err := New(conf, args)
				if err != nil {
					t.Fatalf("error creating container: %v", err)
				}
				defer cont2.Destroy()
				err = cont2.Restore(conf, imagePath, sandbox.RestoreOpts{Paused: true})
				switch {
				case !tc.failed && err != nil:
					t.Fatalf("error restoring container paused: %v", err)
				case !tc.failed:
					if got, want := cont2.State().Status, Paused; got != want {
						t.Fatalf("restored container status: got %v, want %v", got, want)
					}
				default:
					t.Logf("restore: %v; status: %v", err, cont2.State().Status)
				}

				sandboxPid := 0
				if cont2.Sandbox != nil {
					sandboxPid = cont2.Sandbox.Getpid()
				}
				killed := make(chan error, 1)
				go func() { killed <- cont2.SignalContainer(unix.SIGKILL, true /* all */) }()
				select {
				case err := <-killed:
					if err != nil && !tc.failed {
						t.Errorf("error killing the container restored paused: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("killing the container restored paused took more than 10 s")
				}
				destroyed := make(chan error, 1)
				go func() { destroyed <- cont2.Destroy() }()
				select {
				case err := <-destroyed:
					if err != nil {
						t.Errorf("error destroying the killed container: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("destroying the killed container took more than 10 s")
				}
				if sandboxPid != 0 {
					if err := sandboxExits(sandboxPid, 10*time.Second); err != nil {
						t.Error(err)
					}
				}
			})
		}
	}
}
