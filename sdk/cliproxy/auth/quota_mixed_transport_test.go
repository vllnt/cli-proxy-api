package auth

import (
	"context"
	"fmt"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestQuotaMixedTransportUsesExecutorEligibleProviders(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		for _, extraExecutor := range []bool{false, true} {
			for _, aware := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/extra-executor=%v/quota=%v", provider, extraExecutor, aware), func(t *testing.T) {
					var selector Selector = &FillFirstSelector{}
					if aware {
						selector = &QuotaAwareSelector{Fallback: selector}
					}
					manager := NewManager(nil, selector, nil)
					manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: provider})
					if extraExecutor {
						// A registered but account-empty provider must still count:
						// native mixed scheduling does not prefer websocket in this case.
						manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "claude"})
					}
					for _, candidate := range []*Auth{
						{ID: "a-http", Provider: provider},
						{ID: "b-ws", Provider: provider, Attributes: map[string]string{"websockets": "true"}},
					} {
						if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
							t.Fatal(errRegister)
						}
					}
					ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
					got, _, gotProvider, errPick := manager.pickNextMixed(ctx, []string{provider, "claude"}, "", cliproxyexecutor.Options{}, nil)
					want := "b-ws"
					if extraExecutor {
						want = "a-http"
					}
					if errPick != nil || got == nil || got.ID != want || gotProvider != provider {
						t.Fatalf("mixed selection = %v, provider=%q, error=%v; want %s from %s", got, gotProvider, errPick, want, provider)
					}
				})
			}
		}
	}
}
