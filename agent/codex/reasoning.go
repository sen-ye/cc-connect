package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
)

type reasoningModel struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	IsDefault bool   `json:"isDefault"`
	Efforts   []struct {
		Effort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
}

type reasoningMetadata struct {
	Model  string
	Models []reasoningModel
}

type reasoningMetadataOptions struct {
	Cmd  string
	Args []string
	Dir  string
	Env  []string
	URL  string
}

type reasoningMetadataCache struct {
	Key     [32]byte
	Expires time.Time
	Data    reasoningMetadata
}

func (a *Agent) modelReasoningEfforts() []string {
	a.mu.Lock()
	model := core.GetProviderModel(a.providers, a.activeIdx, a.model)
	opts := reasoningMetadataOptions{Cmd: a.cmd, Args: append([]string(nil), a.cliExtraArgs...), Dir: a.workDir}
	opts.Env = append(opts.Env, a.configEnv...)
	opts.Env = append(opts.Env, a.providerEnvLocked()...)
	opts.Env = append(opts.Env, a.sessionEnv...)
	if a.codexHome != "" {
		opts.Env = append(opts.Env, "CODEX_HOME="+a.codexHome)
	}
	if a.backend == "app_server" {
		opts.URL = a.appServerURL
	}
	a.mu.Unlock()
	// New sets a work directory even when cmd is empty to enable discovery.
	// Only a zero-value agent has no executable, work directory, or transport.
	if opts.Cmd == "" && opts.Dir == "" && opts.URL == "" {
		return legacyReasoningEfforts()
	}
	encoded, _ := json.Marshal(opts)
	key := sha256.Sum256(encoded)
	a.reasoningMu.Lock()
	defer a.reasoningMu.Unlock()
	if a.reasoningCache.Key != key || time.Now().After(a.reasoningCache.Expires) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		data, err := readReasoningMetadata(ctx, opts)
		cancel()
		if err != nil {
			slog.Warn("codex: model reasoning metadata unavailable", "error", err)
			if a.reasoningCache.Key != key {
				a.reasoningCache = reasoningMetadataCache{Key: key}
			}
			a.reasoningCache.Expires = time.Now().Add(10 * time.Second)
		} else {
			a.reasoningCache = reasoningMetadataCache{Key: key, Expires: time.Now().Add(time.Minute), Data: data}
		}
	}
	data := a.reasoningCache.Data
	if model == "" {
		model = data.Model
	}
	for _, m := range data.Models {
		if model == m.Model || model == m.ID || (model == "" && m.IsDefault) {
			var levels []string
			seen := map[string]bool{}
			for _, e := range m.Efforts {
				level := normalizeReasoningEffort(e.Effort)
				if level != "" && !seen[level] {
					levels = append(levels, level)
					seen[level] = true
				}
			}
			return levels
		}
	}
	// CLIs without model/list use the upstream compatibility list. A failed
	// refresh above retains the last successful model-specific capabilities.
	return legacyReasoningEfforts()
}

func legacyReasoningEfforts() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
}

type metadataRequest func(string, any, any) error

func readReasoningMetadata(ctx context.Context, opts reasoningMetadataOptions) (reasoningMetadata, error) {
	request, closeRPC, err := openMetadataRPC(ctx, opts)
	if err != nil {
		return reasoningMetadata{}, err
	}
	defer closeRPC()
	if err = request("initialize", map[string]any{"clientInfo": map[string]any{"name": "cc-connect-model-metadata", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}, nil); err != nil {
		return reasoningMetadata{}, err
	}
	if err = request("initialized", map[string]any{}, nil); err != nil {
		return reasoningMetadata{}, err
	}
	var config struct {
		Config struct {
			Model string `json:"model"`
		} `json:"config"`
	}
	// model/list can still work on servers without config/read.
	if err = request("config/read", map[string]any{"includeLayers": false, "cwd": opts.Dir}, &config); err != nil {
		slog.Debug("codex: model metadata config read failed", "error", err)
	}
	data := reasoningMetadata{Model: config.Config.Model}
	cursor := ""
	for page := 0; page < 20; page++ {
		var result struct {
			Data       []reasoningModel `json:"data"`
			NextCursor string           `json:"nextCursor"`
		}
		params := map[string]any{"includeHidden": true}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err = request("model/list", params, &result); err != nil {
			return reasoningMetadata{}, err
		}
		data.Models = append(data.Models, result.Data...)
		if result.NextCursor == "" {
			return data, nil
		}
		if result.NextCursor == cursor {
			return reasoningMetadata{}, fmt.Errorf("codex: model/list repeated cursor")
		}
		cursor = result.NextCursor
	}
	return reasoningMetadata{}, fmt.Errorf("codex: model/list exceeded page limit")
}

func openMetadataRPC(ctx context.Context, opts reasoningMetadataOptions) (metadataRequest, func(), error) {
	if strings.HasPrefix(opts.URL, "ws://") || strings.HasPrefix(opts.URL, "wss://") {
		return openMetadataWebSocket(ctx, opts)
	}
	return openMetadataStdio(ctx, opts)
}

func openMetadataWebSocket(ctx context.Context, opts reasoningMetadataOptions) (metadataRequest, func(), error) {
	var id int64
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, opts.URL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("codex metadata websocket: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	request := func(method string, params, out any) error {
		if method == "initialized" {
			return conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		}
		id++
		if err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			return err
		}
		for {
			var response rpcResponseEnvelope
			if err := conn.ReadJSON(&response); err != nil {
				return err
			}
			got, ok := rpcIDToInt64(response.ID)
			if !ok || got != id {
				continue
			}
			if response.Error != nil {
				return fmt.Errorf("%s: %s", method, response.Error.Message)
			}
			if out != nil {
				return json.Unmarshal(response.Result, out)
			}
			return nil
		}
	}
	return request, func() { stop(); _ = conn.Close() }, nil
}

func openMetadataStdio(ctx context.Context, opts reasoningMetadataOptions) (metadataRequest, func(), error) {
	var id int64

	bin, err := resolveCodexExecutable(opts.Cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("codex reasoning metadata resolve CLI: %w", err)
	}
	args := append(append([]string(nil), opts.Args...), "app-server")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = opts.Dir
	cmd.Env = core.MergeEnv(os.Environ(), opts.Env)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = stdin.Close(); _ = stdout.Close() })
	reader := bufio.NewReader(stdout)
	request := func(method string, params, out any) error {
		if method == "initialized" {
			return rpcNotifyOverIO(stdin, method, params)
		}
		id++
		return rpcRequestOverIO(stdin, reader, id, method, params, out)
	}
	return request, func() { stop(); _ = stdin.Close(); _ = stdout.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }, nil
}
