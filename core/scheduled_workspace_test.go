package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type scheduledTopicPlatform struct{ stubPlatformEngine }

func (p *scheduledTopicPlatform) ChannelKeyFromSessionKey(key string) (string, bool) {
	for _, topic := range []string{"a", "b"} {
		if key == "test:room:branch:"+topic {
			return "room:topic:" + topic, true
		}
	}
	return "", false
}
func (*scheduledTopicPlatform) ReconstructReplyCtx(key string) (any, error) { return key, nil }

type scheduledWorkspaceAgent struct {
	*providerBindingTestAgent
	name, workDir string
}

func (a *scheduledWorkspaceAgent) Name() string       { return a.name }
func (a *scheduledWorkspaceAgent) GetWorkDir() string { return a.workDir }
func (a *scheduledWorkspaceAgent) StartSessionWithProvider(ctx context.Context, id, provider string) (AgentSession, error) {
	s, err := a.providerBindingTestAgent.StartSessionWithProvider(ctx, id, provider)
	if err == nil {
		s.(*providerBindingTestSession).reply += " workspace=" + a.workDir
	}
	return s, err
}

func newScheduledWorkspaceEngine(t *testing.T, base string) (*Engine, *scheduledTopicPlatform, string, string) {
	t.Helper()
	name := "scheduled-workspace-" + t.Name()
	a, b := normalizeWorkspacePath(filepath.Join(base, "a")), normalizeWorkspacePath(filepath.Join(base, "b"))
	for _, dir := range []string{a, b} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	RegisterAgent(name, func(opts map[string]any) (Agent, error) {
		return &scheduledWorkspaceAgent{newProviderBindingTestAgent("key-b"), name, opts["work_dir"].(string)}, nil
	})
	p := &scheduledTopicPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &scheduledWorkspaceAgent{newProviderBindingTestAgent("key-b"), name, base}, []Platform{p}, filepath.Join(base, "sessions.json"), LangEnglish)
	e.SetMultiWorkspace(base, filepath.Join(base, "bindings.json"))
	t.Cleanup(func() { _ = e.Stop() })
	return e, p, a, b
}

func bindScheduledTopics(e *Engine, a, b string) {
	e.workspaceBindings.Bind("project:test", "test:room:topic:a", "topic A", a)
	e.workspaceBindings.Bind("project:test", "test:room:topic:b", "topic B", b)
	// A different chat-level default must never shadow the topic override.
	e.workspaceBindings.Bind("project:test", "test:room", "room", b)
}

func executeScheduledCheck(e *Engine, kind, key, workDir string) error {
	if kind == "timer" {
		return e.ExecuteTimerJob(&TimerJob{ID: "check", Project: "test", SessionKey: key, Prompt: "check progress", WorkDir: workDir})
	}
	return e.ExecuteCronJob(&CronJob{ID: "check", Project: "test", SessionKey: key, Prompt: "check progress", WorkDir: workDir})
}

func TestScheduledChecks_TopicWorkspaceAndProviderSurviveRestart(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			e, _, a, b := newScheduledWorkspaceEngine(t, base)
			bindScheduledTopics(e, a, b)
			_, sessions, err := e.getOrCreateWorkspaceAgent(a)
			if err != nil {
				t.Fatal(err)
			}
			s := sessions.GetOrCreateActive("test:room:branch:a")
			s.SelectProvider("key-a")
			s.SetAgentSessionID("original-r3-session", e.agent.Name())
			s.AddHistory("user", "original R3 task")
			sessions.Save()
			_ = e.Stop()
			restarted, p, _, _ := newScheduledWorkspaceEngine(t, base)
			if err := executeScheduledCheck(restarted, kind, "test:room:branch:a", ""); err != nil {
				t.Fatal(err)
			}
			text := strings.Join(p.getSent(), "\n")
			for _, want := range []string{"workspace=" + a, "API=key-a", "session=original-r3-session"} {
				if !strings.Contains(text, want) {
					t.Fatalf("scheduled check lost %q: %s", want, text)
				}
			}
			if strings.Contains(text, "workspace="+b) {
				t.Fatal("check used the other topic's workspace")
			}
		})
	}
}

func TestScheduledChecks_WorkspaceFailureDoesNotRunGlobalAgent(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		for _, mode := range []string{"missing", "factory", "explicit"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				e, p, a, b := newScheduledWorkspaceEngine(t, t.TempDir())
				bindScheduledTopics(e, a, b)
				workDir := ""
				if mode == "missing" {
					if err := os.Remove(a); err != nil {
						t.Fatal(err)
					}
				} else {
					RegisterAgent(e.agent.Name(), func(map[string]any) (Agent, error) { return nil, fmt.Errorf("workspace unavailable") })
					if mode == "explicit" {
						workDir = a
					}
				}
				if err := executeScheduledCheck(e, kind, "test:room:branch:a", workDir); err == nil {
					t.Fatal("workspace failure silently ran a different project")
				}
				if strings.Contains(strings.Join(p.getSent(), "\n"), "API=") {
					t.Fatal("global agent executed after workspace resolution failed")
				}
				if binding, _ := e.workspaceBindings.LookupEffective("project:test", "test:room:topic:a"); binding == nil {
					t.Fatal("scheduled lookup deleted the workspace binding")
				}
			})
		}
	}
}

func TestScheduledChecks_DirOverrideAndExplicitWorkDir(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		t.Run(kind, func(t *testing.T) {
			e, p, a, b := newScheduledWorkspaceEngine(t, t.TempDir())
			bindScheduledTopics(e, a, b)
			e.SetProjectStateStore(NewProjectStateStore(filepath.Join(t.TempDir(), "project.json")))
			key := "test:room:branch:a"
			e.projectState.SetWorkspaceDirOverride(a+":"+key, b)
			e.projectState.Save()
			for _, tc := range []struct{ explicit, want string }{{"", b}, {a, a}} {
				p.clearSent()
				if err := executeScheduledCheck(e, kind, key, tc.explicit); err != nil {
					t.Fatal(err)
				}
				if text := strings.Join(p.getSent(), "\n"); !strings.Contains(text, "workspace="+tc.want) {
					t.Fatalf("scheduled working directory priority was lost: %s", text)
				}
			}
		})
	}
}

// Called as a sub-case of the existing topic-workspace CUJ. User actions go
// through ReceiveMessage; firing a saved timer uses the real scheduler.
func scheduledTopicTimerCUJ(t *testing.T) {
	base := t.TempDir()
	e, p, a, b := newScheduledWorkspaceEngine(t, base)
	bindScheduledTopics(e, a, b)
	store, err := NewTimerStore(base)
	if err != nil {
		t.Fatal(err)
	}
	sched := NewTimerScheduler(store)
	sched.RegisterEngine("test", e)
	e.SetTimerScheduler(sched)
	defer sched.Stop()
	key := "test:room:branch:a"
	send := func(content string) {
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), ChannelKey: "room:topic:a", UserID: "user", Content: content, ReplyCtx: key})
	}
	send("/provider switch key-a")
	send("original R3 task")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(strings.Join(p.getSent(), "\n"), "API=key-a") {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(strings.Join(p.getSent(), "\n"), "API=key-a") {
		t.Fatal("initial task did not reach selected provider")
	}
	send("/timer add 2h check progress")
	jobs := store.ListPending()
	if len(jobs) != 1 {
		t.Fatalf("created %d timers", len(jobs))
	}
	sched.Stop()
	_ = e.Stop()
	restarted, replies, _, _ := newScheduledWorkspaceEngine(t, base)
	reloaded, err := NewTimerStore(base)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewTimerScheduler(reloaded)
	runner.RegisterEngine("test", restarted)
	restarted.SetTimerScheduler(runner)
	defer runner.Stop()
	runner.executeJob(jobs[0].ID)
	text := strings.Join(replies.getSent(), "\n")
	for _, want := range []string{"workspace=" + a, "API=key-a", "session=native-key-a-1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("after timer/restart user did not see %q: %s", want, text)
		}
	}
	restarted.ReceiveMessage(replies, &Message{SessionKey: key, Platform: replies.Name(), ChannelKey: "room:topic:a", UserID: "user", Content: "/history", ReplyCtx: key})
	text = strings.Join(replies.getSent(), "\n")
	if !strings.Contains(text, "original R3 task") || !strings.Contains(text, "check progress") {
		t.Fatalf("timer did not continue the original visible history: %s", text)
	}
}
