package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const execHelperArg = "__tun-exec-helper"

func init() {
	if len(os.Args) < 2 || os.Args[1] != execHelperArg {
		return
	}
	// bwrap unconditionally adds PWD after --clearenv/--chdir. Apply the exact
	// action environment only after entering its namespaces, then replace this
	// helper with the command (no egress proxy and no extra process).
	err := exactExec(os.Args[2:])
	fmt.Fprintln(os.Stderr, "tun exec:", err)
	os.Exit(127)
}
func exactExec(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("missing command")
	}
	f := os.NewFile(5, "action-environment")
	data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	f.Close()
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return fmt.Errorf("environment too large")
	}
	var env []string
	if text := strings.TrimSuffix(string(data), "\x00"); text != "" {
		env = strings.Split(text, "\x00")
	}
	os.Clearenv()
	for _, v := range env {
		k, value, ok := strings.Cut(v, "=")
		if !ok {
			return fmt.Errorf("invalid environment")
		}
		if err = os.Setenv(k, value); err != nil {
			return err
		}
	}
	name, err := exec.LookPath(argv[0])
	if errors.Is(err, exec.ErrDot) {
		err = nil
	}
	if err != nil {
		return err
	}
	return syscall.Exec(name, argv, env)
}
