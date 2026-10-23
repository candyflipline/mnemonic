package redact

// Builtin rules 13-24 as hand-written matchers, no regex engine: one shared
// prefix+bounded-run shape (bTailRule), one exact-hex shape (bHexRule),
// and the three patterns whose shape differs (bOpenAI, bSendGrid,
// bDiscordHook), all meeting the matcher contract (see matchers.go).

var (
	m13 = bTailRule("sk-ant-", 20, bClsWordDash)   // anthropic-api-key
	m14 = bOpenAI                                  // openai-api-key
	m15 = bTailRule("hf_", 30, bClsAlnum)          // huggingface-token
	m16 = bSendGrid                                // sendgrid-api-key
	m17 = bDiscordHook                             // discord-webhook
	m18 = bTailRule("npm_", 30, bClsAlnum)         // npm-token
	m19 = bTailRule("pypi-", 30, bClsWordDash)     // pypi-token
	m20 = bHexRule("dop_v1_", 64)                  // digitalocean-token
	m21 = bHexRule("dapi", 32)                     // databricks-token
	m22 = bTailRule("dckr_pat_", 20, bClsWordDash) // dockerhub-token
	m23 = bTailRule("lin_api_", 30, bClsAlnum)     // linear-api-key
	m24 = bTailRule("secret_", 30, bClsAlnum)      // notion-token
)
