package index

import "testing"

func TestParseControl(t *testing.T) {
	const (
		ts    = `"ts":"2026-10-27T22:00:00Z","request_key":"k1"`
		id    = `"link_id":"0123456789ab"`
		token = `"token_sha256":"` + "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" + `"`
		key   = `"key":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"`
		read  = `"action":"link","kind":"read",` + id + `,` + token + `,"session_ids":["s1","s2"],"cutoff":"2026-10-27T22:00:00Z","expires":"2026-11-26T22:00:00Z","author":"chris","tenant":"default",`
		inv   = `"action":"link","kind":"invite",` + id + `,` + token + `,"invite_author":"vlad","expires":"2026-11-03T22:00:00Z","author":"chris","tenant":"default",`
	)
	for _, tc := range []struct{ line, err string }{
		{`{"session_id":"s1","action":"delete",` + ts + `}`, ""},
		{`{"session_id":"s1","action":"visibility","visibility":"private",` + ts + `}`, ""},
		{`{` + read + `"title":"QUIC context","note":"n",` + ts + `}`, ""},
		{`{` + inv + ts + `}`, ""},
		{`{"action":"join",` + id + `,` + key + `,"machine":"vlad-mbp",` + ts + `}`, ""},
		{`{"action":"revoke",` + id + `,` + ts + `}`, ""},

		{`{"session_id":"s1","action":"visibility","visibility":"link",` + ts + `}`, `control record visibility "link"`},
		{`{"session_id":"s1","action":"share",` + ts + `}`, `control record action "share" kind ""`},
		{`{"session_id":"s1","action":"delete","kind":"read",` + ts + `}`, `control record action "delete" kind "read"`},
		{`{"action":"delete",` + ts + `}`, `control record delete lacks session_id`},
		{`{"session_id":"s1","action":"delete"}`, `control record lacks request_key or ts`},
		{`{` + read + `"invite_author":"vlad",` + ts + `}`, `control record link/read carries invite_author`},
		{`{` + inv + `"session_ids":["s1"],` + ts + `}`, `control record link/invite carries session_ids`},
		{`{` + inv + `"title":"t",` + ts + `}`, `control record link/invite carries title`},
		{`{"action":"link","kind":"write",` + id + `,` + ts + `}`, `control record action "link" kind "write"`},
		{`{"action":"link","kind":"read",` + id + `,` + token + `,"cutoff":"2026-10-27T22:00:00Z","expires":"2026-11-26T22:00:00Z","author":"chris","tenant":"default",` + ts + `}`, `control record link/read lacks session_ids`},
		{`{` + read + `"session_ids":[],` + ts + `}`, `control record session_ids holds an empty id or none`},
		{`{"action":"join",` + id + `,` + key + `,` + ts + `}`, `control record join lacks machine`},
		{`{"action":"join",` + id + `,"key":"short","machine":"m",` + ts + `}`, `control record: key "short" must be unpadded base64url of 32 bytes`},
		{`{"action":"join",` + id + `,` + key + `,"machine":"Vlad MBP",` + ts + `}`, `control record machine "Vlad MBP"`},
		{`{"action":"revoke","link_id":"0123456789a",` + ts + `}`, `control record link_id "0123456789a" is not 12 hex digits`},
		{`{"action":"revoke","link_id":"0123456789AB",` + ts + `}`, `control record link_id "0123456789AB" is not 12 hex digits`},
		{`{"action":"revoke",` + id + `,"session_id":"s1",` + ts + `}`, `control record revoke carries session_id`},
		{`{"action":"link","kind":"invite",` + id + `,"token_sha256":"abc","invite_author":"vlad","expires":"2026-11-03T22:00:00Z","author":"chris","tenant":"default",` + ts + `}`, `control record token_sha256 is not 64 hex digits`},
		{`{` + inv[:len(inv)-len(`"invite_author":"vlad","expires":"2026-11-03T22:00:00Z","author":"chris","tenant":"default",`)] + `"invite_author":"../x","expires":"2026-11-03T22:00:00Z","author":"chris","tenant":"default",` + ts + `}`, `control record invite_author "../x"`},
	} {
		_, err := parseControl([]byte(tc.line))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.err {
			t.Errorf("%s\n got error %q\nwant error %q", tc.line, got, tc.err)
		}
	}
}
