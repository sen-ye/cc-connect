package codex

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// RunProviderCLI runs a foreground native CLI with the same isolated provider
// and recovery proxy as bridge sessions. It writes no shared Codex config and
// keeps the proxy alive until the CLI exits. The caller owns terminal signals.
func RunProviderCLI(ctx context.Context, binary string, args []string, provider core.ProviderConfig, stdin io.Reader, stdout, stderr io.Writer) error {
	provider = cloneProvider(provider)
	runtime, err := prepareProviderRuntime(&provider, providerEnvironment(provider))
	if err != nil {
		return err
	}
	defer runtime.proxy.Close()
	// An ephemeral proxy must not be attached to the shared background daemon.
	prefix := append(runtime.args, "--no-daemon", "-c", "model_provider="+tomlString(provider.Name))
	if provider.Model != "" {
		prefix = append(prefix, "-c", "model="+tomlString(provider.Model))
	}
	cmd := exec.CommandContext(ctx, binary, append(prefix, args...)...)
	cmd.Cancel = func() error { return terminateProviderCLI(cmd) }
	cmd.Env = core.MergeEnv(os.Environ(), runtime.env)
	if runtime.baseURL != "" {
		cmd.Env = core.MergeEnv(cmd.Env, []string{"OPENAI_BASE_URL=" + runtime.baseURL})
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codex: provider CLI: %w", err)
	}
	return nil
}
