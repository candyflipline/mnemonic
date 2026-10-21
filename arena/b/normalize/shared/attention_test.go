package shared

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShared(t *testing.T) {
	t.Run("payload_uid_rewrite", func(t *testing.T) {
		for _, tc := range []struct{ name, in, want string }{
			{"absent", `{"text":"unchanged"}`, `{"text":"unchanged"}`},
			{"literal", `{"request_event_uid":"old"}`, `{"request_event_uid":"new"}`},
			{"escaped", `{"request\u005fevent_uid":"old"}`, `{"request_event_uid":"new"}`},
			{"nested", `{ "nested": {"request_event_uid":"old"}}`, `{ "nested": {"request_event_uid":"old"}}`},
			{"value", `{"text":"request_event_uid"}`, `{"text":"request_event_uid"}`},
			{"malformed", `{"text":`, `{"text":`},
			{"unmapped", `{ "request_event_uid": "missing" }`, `{ "request_event_uid": "missing" }`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := &EventRow{}
				ev.Event.PayloadJSON = tc.in
				rewritePayloadUID(ev, map[string]string{"old": "new"})
				if ev.Event.PayloadJSON != tc.want {
					t.Fatalf("payload=%q want=%q", ev.Event.PayloadJSON, tc.want)
				}
			})
		}
	})
	root := t.TempDir()
	marker := filepath.Join(root, ".git")
	if err := os.Mkdir(marker, 0700); err != nil {
		t.Fatal(err)
	}
	ResetAttentionCache()
	for _, tc := range []struct{ name, cwd, prefix, wantRoot string }{
		{"git", root, "git:", root},
		{"missing", filepath.Join(root, "missing", "child"), "git:", root},
		{"relative", "relative", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := EventFileAttentionFields(tc.cwd)
			if v.WorktreeRoot != tc.wantRoot || !strings.HasPrefix(v.ProjectID, tc.prefix) || v.RepoRelPath != "" {
				t.Fatalf("wrong path fields: %+v", v)
			}
			if tc.prefix != "" && len(v.ProjectID) != len(tc.prefix)+64 {
				t.Fatal("missing project identity")
			}
		})
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if v := EventFileAttentionFields(root); !strings.HasPrefix(v.ProjectID, "git:") {
		t.Fatal("snapshot did not retain original lookup")
	}
	ResetAttentionCache()
	if v := EventFileAttentionFields(root); !strings.HasPrefix(v.ProjectID, "dir:") || v.WorktreeRoot != root {
		t.Fatal("next snapshot failed to refresh filesystem")
	}
}
