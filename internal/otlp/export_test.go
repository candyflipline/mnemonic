package otlp

import "time"

func SetReadTimeout(d time.Duration) { otlpReadTimeout = d }
