package redact

var (
	m00 = token(true, prefixAWS, classAWS, 16, 16, true)                                        // aws-access-token
	m01 = token(true, prefixGitHub, classWord, 36, 255, true)                                   // github-token
	m02 = token(true, litPrefix("github_pat_"), classWord, 82, 255, true)                       // github-fine-grained-token
	m03 = token(true, prefixGitlab, classWordDash, 20, 0, true)                                 // gitlab-token
	m04 = token(true, prefixSlack, classAlnumDash, 10, 0, true)                                 // slack-token
	m05 = token(false, litPrefix("https://hooks.slack.com/services/"), classHook, 20, 0, false) // slack-webhook
	m06 = token(true, litPrefix("AIza"), classWordDash, 35, 35, true)                           // google-api-key
	m07 = token(true, litPrefix("ya29."), classWordDash, 20, 0, true)                           // google-oauth-token
	m08 = pem("-----BEGIN ", "-----END ", "PRIVATE KEY-----")                                   // pem-private-key
	m09 = pem("-----BEGIN OPENSSH PRIVATE KEY-----", "-----END OPENSSH PRIVATE KEY-----", "")   // openssh-private-key
	m10 = jwt                                                                                   // jwt
	m11 = token(true, prefixStripe, classAlnum, 10, 0, true)                                    // stripe-token
	m12 = token(true, litPrefix("whsec_"), classAlnum, 20, 0, true)                             // stripe-webhook-secret
)

// token fixes one rule's shape into a matcher for scanToken: whether a \b
// precedes the head, the head itself, the run class and its bounds, and
// whether a \b follows the run.
func token(lead bool, prefix prefixFn, class classFn, minRun, maxRun int, trail bool) matcher {
	return func(text []byte, out [][2]int) [][2]int {
		return scanToken(text, out, lead, prefix, class, minRun, maxRun, trail)
	}
}

// pem fixes one rule's PEM literals into a matcher for scanPEM. With a
// non-empty tail the kind word precedes it (rule 8); an empty tail means
// the literals are whole (rule 9).
func pem(begin, end, tail string) matcher {
	b, e, t := []byte(begin), []byte(end), []byte(tail)
	kind := tail != ""
	return func(text []byte, out [][2]int) [][2]int {
		return scanPEM(text, out, b, e, kind, t)
	}
}

func jwt(text []byte, out [][2]int) [][2]int {
	return scanJWT(text, out)
}
