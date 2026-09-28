package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestReasoningEfforts_ModelCapabilities(t *testing.T) {
	a := &Agent{cmd: os.Args[0], cliExtraArgs: []string{"-test.run=TestReasoningMetadataHelper", "--"}, workDir: t.TempDir(), configEnv: []string{"CC_REASONING_HELPER=1"}, activeIdx: -1}
	if got := a.AvailableReasoningEfforts(); !slices.Equal(got, []string{"low", "high", "max", "ultra"}) {
		t.Fatalf("default model efforts = %v", got)
	}
	a.SetModel("small-model")
	if got := a.AvailableReasoningEfforts(); !slices.Equal(got, []string{"low", "high"}) {
		t.Fatalf("small model efforts = %v", got)
	}
	a.SetModel("no-reasoning-model")
	if got := a.AvailableReasoningEfforts(); len(got) != 0 {
		t.Fatalf("model without reasoning advertised %v", got)
	}
}

func TestReasoningEffort_UltraPreserved(t *testing.T) {
	a, err := New(map[string]any{"cmd": os.Args[0], "work_dir": t.TempDir(), "reasoning_effort": " ULTRA "})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.(*Agent).GetReasoningEffort(); got != "ultra" {
		t.Fatalf("configured effort = %q, want ultra", got)
	}
}

func TestReasoningMetadataHelper(t *testing.T) {
	if os.Getenv("CC_REASONING_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if os.Getenv("CC_REASONING_WAIT") == "1" {
			time.Sleep(time.Minute)
			continue
		}
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var result any = map[string]any{}
		switch req.Method {
		case "config/read":
			result = map[string]any{"config": map[string]any{"model": "large-model"}}
		case "model/list":
			models := []any{}
			for name, levels := range map[string][]string{"large-model": {"low", "high", "max", "ultra"}, "small-model": {"low", "high"}, "no-reasoning-model": {}} {
				efforts := []any{}
				for _, level := range levels {
					efforts = append(efforts, map[string]any{"reasoningEffort": level})
				}
				models = append(models, map[string]any{"id": name, "model": name, "supportedReasoningEfforts": efforts, "isDefault": name == "large-model"})
			}
			result = map[string]any{"data": models, "nextCursor": nil}
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"id": req.ID, "result": result})
	}
	os.Exit(0)
}

func TestReasoningMetadata_RemotePagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var req struct {
				ID     any            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if conn.ReadJSON(&req) != nil {
				return
			}
			if req.ID == nil {
				continue
			}
			var result any = map[string]any{}
			switch req.Method {
			case "config/read":
				result = map[string]any{"config": map[string]any{"model": "remote-model"}}
			case "model/list":
				if req.Params["cursor"] == nil {
					result = map[string]any{"data": []any{}, "nextCursor": "page-two"}
				} else {
					result = map[string]any{"data": []any{map[string]any{"model": "remote-model", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}, map[string]any{"reasoningEffort": "ultra"}}}}}
				}
			}
			if conn.WriteJSON(map[string]any{"id": req.ID, "result": result}) != nil {
				return
			}
		}
	}))
	defer server.Close()
	a := &Agent{cmd: "not-a-local-cli", backend: "app_server", appServerURL: "ws" + strings.TrimPrefix(server.URL, "http"), activeIdx: -1}
	if got := a.AvailableReasoningEfforts(); !slices.Equal(got, []string{"high", "ultra"}) {
		t.Fatalf("remote efforts = %v", got)
	}
}

func TestReasoningMetadata_TimeoutClosesProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := readReasoningMetadata(ctx, reasoningMetadataOptions{Cmd: os.Args[0], Args: []string{"-test.run=TestReasoningMetadataHelper", "--"}, Dir: t.TempDir(), Env: []string{"CC_REASONING_HELPER=1", "CC_REASONING_WAIT=1"}})
	if err == nil {
		t.Fatal("silent server should time out")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("metadata cancellation did not close the process promptly")
	}
}

func TestNativeReasoningMetadata(t *testing.T) {
	if os.Getenv("CC_CONNECT_NATIVE_TESTS") != "1" {
		t.Skip("opt-in local CLI metadata check")
	}
	a, err := New(map[string]any{"cmd": "codex", "work_dir": t.TempDir(), "model": "gpt-6-astra"})
	if err != nil {
		t.Fatal(err)
	}
	agent := a.(*Agent)
	if got := agent.AvailableReasoningEfforts(); !slices.Contains(got, "ultra") {
		t.Fatalf("Astra efforts: %v", got)
	}
	agent.SetModel("gpt-6-luna")
	if got := agent.AvailableReasoningEfforts(); slices.Contains(got, "ultra") || !slices.Contains(got, "max") {
		t.Fatalf("Luna efforts: %v", got)
	}
}
