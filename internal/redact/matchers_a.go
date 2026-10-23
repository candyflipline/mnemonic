package redact

// Builtin rules 0-12, regex-backed. Lane U2b replaces these entries with
// hand-written matchers meeting the matcher contract (see matchers.go).

var (
	m00 = regexEntry(0)  // aws-access-token
	m01 = regexEntry(1)  // github-token
	m02 = regexEntry(2)  // github-fine-grained-token
	m03 = regexEntry(3)  // gitlab-token
	m04 = regexEntry(4)  // slack-token
	m05 = regexEntry(5)  // slack-webhook
	m06 = regexEntry(6)  // google-api-key
	m07 = regexEntry(7)  // google-oauth-token
	m08 = regexEntry(8)  // pem-private-key
	m09 = regexEntry(9)  // openssh-private-key
	m10 = regexEntry(10) // jwt
	m11 = regexEntry(11) // stripe-token
	m12 = regexEntry(12) // stripe-webhook-secret
)
