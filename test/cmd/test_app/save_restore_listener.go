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

package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"os"

	"github.com/google/subcommands"
	"gvisor.dev/gvisor/runsc/flag"
)

// saveRestoreListener listens on a save/restore socket (runsc checkpoint
// --save-restore-exec-socket), as a container's process that a save or a
// restore asks to ready itself does.
type saveRestoreListener struct {
	socket string
	log    string
	answer string
}

// Name implements subcommands.Command.Name.
func (*saveRestoreListener) Name() string { return "save-restore-listener" }

// Synopsis implements subcommands.Command.Synopsis.
func (*saveRestoreListener) Synopsis() string {
	return "listens on a save/restore socket, logging each request and answering it"
}

// Usage implements subcommands.Command.Usage.
func (*saveRestoreListener) Usage() string {
	return "save-restore-listener --socket=<path> --log=<path> [--answer=<line>]\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (c *saveRestoreListener) SetFlags(f *flag.FlagSet) {
	f.StringVar(&c.socket, "socket", "", "path of the socket to listen on")
	f.StringVar(&c.log, "log", "", "path of the file that \"listening\", then each request's line, is appended to")
	f.StringVar(&c.answer, "answer", "ok", "line answered to each request")
}

// Execute implements subcommands.Command.Execute.
func (c *saveRestoreListener) Execute(ctx context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if c.socket == "" || c.log == "" {
		log.Print("--socket and --log must be set")
		return subcommands.ExitUsageError
	}
	out, err := os.OpenFile(c.log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		log.Fatalf("opening the log: %v", err)
	}
	listener, err := net.Listen("unix", c.socket)
	if err != nil {
		log.Fatalf("listening on %q: %v", c.socket, err)
	}
	if _, err := fmt.Fprintln(out, "listening"); err != nil {
		log.Fatalf("writing the log: %v", err)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Fatalf("accepting a connection: %v", err)
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			log.Fatalf("reading a request: %v", err)
		}
		if _, err := out.WriteString(line); err != nil {
			log.Fatalf("writing the log: %v", err)
		}
		if _, err := fmt.Fprintln(conn, c.answer); err != nil {
			log.Fatalf("answering: %v", err)
		}
		conn.Close()
	}
}
