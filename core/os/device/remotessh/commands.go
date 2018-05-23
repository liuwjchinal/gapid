// Copyright (C) 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remotessh

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/google/gapid/core/app/crash"
	"github.com/google/gapid/core/log"
	"github.com/google/gapid/core/os/device"
	"github.com/google/gapid/core/os/shell"
	"golang.org/x/crypto/ssh"
)

// remoteProcess is the interface to a running process, as started by a Target.
type remoteProcess struct {
	session *ssh.Session
	wg      sync.WaitGroup
}

func (r *remoteProcess) Kill() error {
	return r.session.Signal(ssh.SIGSEGV)
}

func (r *remoteProcess) Wait(ctx context.Context) error {
	ret := r.session.Wait()
	r.wg.Wait()
	return ret
}

var _ shell.Process = (*remoteProcess)(nil)

type sshShellTarget struct{ b *binding }

// Start starts the given command in the remote shell.
func (t sshShellTarget) Start(cmd shell.Cmd) (shell.Process, error) {
	session, err := t.b.connection.NewSession()
	if err != nil {
		return nil, err
	}
	p := &remoteProcess{
		session: session,
		wg:      sync.WaitGroup{},
	}

	if cmd.Stdin != nil {
		stdin, err := session.StdinPipe()
		if err != nil {
			return nil, err
		}
		crash.Go(func() {
			defer stdin.Close()
			io.Copy(stdin, cmd.Stdin)
		})
	}

	if cmd.Stdout != nil {
		stdout, err := session.StdoutPipe()
		if err != nil {
			return nil, err
		}
		p.wg.Add(1)
		crash.Go(func() {
			io.Copy(cmd.Stdout, stdout)
			p.wg.Done()
		})
	}

	if cmd.Stderr != nil {
		stderr, err := session.StderrPipe()
		if err != nil {
			return nil, err
		}
		p.wg.Add(1)
		crash.Go(func() {
			io.Copy(cmd.Stderr, stderr)
			p.wg.Done()
		})
	}

	prefix := ""
	if cmd.Dir != "" {
		prefix += "cd " + cmd.Dir + "; "
	}

	for _, e := range cmd.Environment.Keys() {
		if e != "" {
			prefix = prefix + e + "=" + cmd.Environment.Get(e) + " "
		}
	}

	if err := session.Start(prefix + cmd.Name + " " + strings.Join(cmd.Args, " ")); err != nil {
		return nil, err
	}

	return p, nil
}

func (t sshShellTarget) String() string {
	c := t.b.configuration
	return c.User + "@" + c.Host + ": " + t.b.String()
}

// Shell implements the Device interface returning commands that will error if run.
func (b binding) Shell(name string, args ...string) shell.Cmd {
	return shell.Command(name, args...).On(sshShellTarget{&b})
}

func (b binding) destroyPosixDirectory(ctx context.Context, dir string) {
	_, _ = b.Shell("rm", "-rf", dir).Call(ctx)
}

func (b binding) createPosixTempDirectory(ctx context.Context) (string, func(context.Context), error) {
	dir, err := b.Shell("mktemp", "-d").Call(ctx)
	if err != nil {
		return "", nil, err
	}
	return dir, func(ctx context.Context) { b.destroyPosixDirectory(ctx, dir) }, nil
}

func (b binding) createWindowsTempDirectory(ctx context.Context) (string, func(ctx context.Context), error) {
	return "", nil, fmt.Errorf("Not yet supported, windows remote targets")
}

// MakeTempDir creates a temporary directory on the remote machine. It returns the
// full path, and a function that can be called to clean up the directory.
func (b binding) MakeTempDir(ctx context.Context) (string, func(ctx context.Context), error) {
	switch b.os {
	case device.Linux:
		fallthrough
	case device.OSX:
		return b.createPosixTempDirectory(ctx)
	case device.Windows:
		return b.createWindowsTempDirectory(ctx)
	default:
		panic(fmt.Errorf("Unsupported OS %v", b.os))
	}
}

// WriteFile moves the contents of io.Reader into the given file on the remote machine.
// The file is given the mode as described by the unix filemode string.
func (b binding) WriteFile(ctx context.Context, contents io.Reader, mode string, destPath string) error {
	_, err := b.Shell("cat", ">", destPath, "; chmod ", mode, " ", destPath).Read(contents).Call(ctx)
	return err
}

// PushFile copies a file from a local path to the remote machine. Permissions are
// maintained across.
func (b binding) PushFile(ctx context.Context, source, dest string) error {
	infile, err := os.Open(source)
	if err != nil {
		return err
	}
	permission, err := os.Stat(source)
	if err != nil {
		return err
	}
	perm := fmt.Sprintf("%4o", permission.Mode().Perm())

	return b.WriteFile(ctx, infile, perm, dest)
}

// doTunnel tunnels a single connection through the SSH connection.
func (b binding) doTunnel(ctx context.Context, local net.Conn, remotePort int) error {
	remote, err := b.connection.Dial("tcp", fmt.Sprintf("localhost:%d", remotePort))
	if err != nil {
		local.Close()
		return err
	}

	wg := sync.WaitGroup{}

	copy := func(writer io.Writer, reader io.Reader) {
		_, err := io.Copy(writer, reader)
		if err != nil {
			log.E(ctx, "Copy Error %s", err)
		}
		wg.Done()
	}

	wg.Add(2)
	crash.Go(func() { copy(local, remote) })
	crash.Go(func() { copy(remote, local) })

	crash.Go(func() {
		defer local.Close()
		defer remote.Close()
		wg.Wait()
	})
	return nil
}

// ForwardPort forwards a local TCP port to the remote machine on the remote port.
// The local port that was opened is returned.
func (b binding) ForwardPort(ctx context.Context, remotePort int) (int, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	crash.Go(func() {
		defer listener.Close()
		for {
			local, err := listener.Accept()
			if err != nil {
				return
			}
			b.doTunnel(ctx, local, remotePort)
		}
	})

	return listener.Addr().(*net.TCPAddr).Port, nil
}
