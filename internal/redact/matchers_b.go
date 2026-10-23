package redact

// Builtin rules 13-24, regex-backed. Lane U2c replaces these entries with
// hand-written matchers meeting the matcher contract (see matchers.go).

var (
	m13 = regexEntry(13) // anthropic-api-key
	m14 = regexEntry(14) // openai-api-key
	m15 = regexEntry(15) // huggingface-token
	m16 = regexEntry(16) // sendgrid-api-key
	m17 = regexEntry(17) // discord-webhook
	m18 = regexEntry(18) // npm-token
	m19 = regexEntry(19) // pypi-token
	m20 = regexEntry(20) // digitalocean-token
	m21 = regexEntry(21) // databricks-token
	m22 = regexEntry(22) // dockerhub-token
	m23 = regexEntry(23) // linear-api-key
	m24 = regexEntry(24) // notion-token
)
