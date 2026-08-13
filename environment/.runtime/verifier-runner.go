// verifier-runner is a tiny image-runtime privilege boundary.  The container's
// long-lived process starts it as the predeclared verifier UID, while Harbor
// invokes the trusted test driver as root.  A root peer may ask the runner to
// execute one verifier binary without requiring setuid(2), chown(2), a user
// namespace, or optional filesystem-sandbox syscalls at grading time.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	socketPath       = "/run/traceweave-verifier/runner.sock"
	maxRequestBytes  = 64 << 10
	maxResponseBytes = 8 << 20
)

type request struct {
	Args []string `json:"args"`
}

type response struct {
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
	full   bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.full = true
		return original, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		b.full = true
	}
	_, _ = b.buffer.Write(data)
	return original, nil
}

func main() {
	if filepath.Base(os.Args[0]) == "sleep" && !(len(os.Args) == 2 && os.Args[1] == "infinity") {
		if err := syscall.Exec("/usr/bin/sleep", append([]string{"sleep"}, os.Args[1:]...), os.Environ()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(125)
		}
	}
	if len(os.Args) >= 2 && os.Args[1] == "run" {
		runClient(os.Args[2:])
		return
	}
	serve()
}

func serve() {
	if os.Getuid() != 1001 || os.Getgid() != 1001 {
		fmt.Fprintln(os.Stderr, "traceweave verifier runner must start as uid/gid 1001")
		os.Exit(125)
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
	if err := os.Chmod(socketPath, 0o666); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
	for {
		connection, err := listener.Accept()
		if err != nil {
			continue
		}
		go handle(connection)
	}
}

func handle(connection net.Conn) {
	defer connection.Close()
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return
	}
	uid, err := peerUID(unixConnection)
	if err != nil || uid != 0 {
		_ = json.NewEncoder(connection).Encode(response{ExitCode: 125, Error: "runner accepts only the root verifier peer"})
		return
	}
	decoder := json.NewDecoder(&limitedReader{reader: connection, remaining: maxRequestBytes})
	decoder.DisallowUnknownFields()
	var request request
	if err := decoder.Decode(&request); err != nil {
		_ = json.NewEncoder(connection).Encode(response{ExitCode: 125, Error: "invalid runner request"})
		return
	}
	if err := validateCommand(request.Args); err != nil {
		_ = json.NewEncoder(connection).Encode(response{ExitCode: 125, Error: err.Error()})
		return
	}

	command := exec.Command(request.Args[0], request.Args[1:]...)
	command.Env = []string{
		"HOME=/nonexistent",
		"LOGNAME=traceweave-verifier",
		"PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TZ=UTC",
		"USER=traceweave-verifier",
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout := &limitedBuffer{limit: maxResponseBytes}
	stderr := &limitedBuffer{limit: maxResponseBytes}
	command.Stdout = stdout
	command.Stderr = stderr

	done := make(chan error, 1)
	if err := command.Start(); err != nil {
		_ = json.NewEncoder(connection).Encode(response{ExitCode: 125, Error: err.Error()})
		return
	}
	go func() { done <- command.Wait() }()
	var runError error
	select {
	case runError = <-done:
	case <-time.After(360 * time.Second):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		runError = <-done
		if runError == nil {
			runError = errors.New("verifier runner timeout")
		}
	}
	killOtherVerifierProcesses()
	cleanupCaseContents(request.Args[4])
	exitCode := 0
	if runError != nil {
		exitCode = 1
		var exitError *exec.ExitError
		if errors.As(runError, &exitError) && exitError.ExitCode() > 0 {
			exitCode = exitError.ExitCode()
		}
	}
	response := response{Stdout: stdout.buffer.Bytes(), Stderr: stderr.buffer.Bytes(), ExitCode: exitCode}
	if stdout.full || stderr.full {
		response.ExitCode = 125
		response.Error = "verifier output exceeded the runner limit"
	} else if runError != nil {
		response.Error = runError.Error()
	}
	_ = json.NewEncoder(connection).Encode(response)
}

func killOtherVerifierProcesses() {
	self := os.Getpid()
	for attempt := 0; attempt < 8; attempt++ {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return
		}
		found := false
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 1 || pid == self {
				continue
			}
			status, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
			if err != nil || !bytes.Contains(status, []byte("\nUid:\t1001\t")) {
				continue
			}
			found = true
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if !found {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func cleanupCaseContents(caseRoot string) {
	entries, err := os.ReadDir(caseRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(caseRoot, entry.Name()))
	}
}

func validateCommand(arguments []string) error {
	if len(arguments) != 5 {
		return errors.New("runner requires the verifier and four fixed arguments")
	}
	clean := filepath.Clean(arguments[0])
	scratch := filepath.Dir(clean)
	if filepath.Dir(scratch) != "/var/lib/traceweave-verifier" ||
		!strings.HasPrefix(filepath.Base(scratch), ".traceweave-verifier.") || filepath.Base(clean) != "verifier" {
		return errors.New("runner command is outside the verifier scratch root")
	}
	if info, err := os.Stat(clean); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o555 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		return errors.New("runner verifier executable is not a protected root file")
	}
	caseRoot := filepath.Clean(arguments[4])
	if filepath.Dir(caseRoot) != scratch || filepath.Base(caseRoot) != "cases" {
		return errors.New("runner case root is outside the verifier scratch root")
	}
	for _, binary := range arguments[1:4] {
		cleanBinary := filepath.Clean(binary)
		if filepath.Dir(cleanBinary) != filepath.Join(scratch, "binaries") {
			return errors.New("runner candidate binary is outside the verifier scratch root")
		}
		info, err := os.Stat(cleanBinary)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o555 || info.Sys().(*syscall.Stat_t).Uid != 0 {
			return errors.New("runner candidate binary is not a protected root file")
		}
	}
	for _, argument := range arguments {
		if strings.IndexByte(argument, 0) >= 0 || len(argument) > 4096 {
			return errors.New("runner argument is invalid")
		}
	}
	return nil
}

func peerUID(connection *net.UnixConn) (uint32, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credential *syscall.Ucred
	var controlError error
	err = raw.Control(func(fd uintptr) {
		credential, controlError = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if controlError != nil {
		return 0, controlError
	}
	return credential.Uid, nil
}

func runClient(arguments []string) {
	if len(arguments) > 0 && arguments[0] == "--" {
		arguments = arguments[1:]
	}
	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "traceweave verifier runner:", err)
		os.Exit(125)
	}
	defer connection.Close()
	if err := json.NewEncoder(connection).Encode(request{Args: arguments}); err != nil {
		fmt.Fprintln(os.Stderr, "traceweave verifier runner:", err)
		os.Exit(125)
	}
	var response response
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		fmt.Fprintln(os.Stderr, "traceweave verifier runner:", err)
		os.Exit(125)
	}
	_, _ = os.Stdout.Write(response.Stdout)
	_, _ = os.Stderr.Write(response.Stderr)
	if response.Error != "" && response.ExitCode != 0 {
		fmt.Fprintln(os.Stderr, "traceweave verifier runner:", response.Error)
	}
	if response.ExitCode < 0 || response.ExitCode > 125 {
		response.ExitCode = 125
	}
	os.Exit(response.ExitCode)
}

type limitedReader struct {
	reader    net.Conn
	remaining int
}

func (r *limitedReader) Read(buffer []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("runner request exceeded " + strconv.Itoa(maxRequestBytes) + " bytes")
	}
	if len(buffer) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	read, err := r.reader.Read(buffer)
	r.remaining -= read
	return read, err
}
