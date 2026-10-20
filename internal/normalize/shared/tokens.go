package shared

var tokenBucketKeys = []string{
	"embedding_input_image",
	"embedding_input_text",
	"input_audio",
	"input_cache_read",
	"input_cache_write",
	"input_image",
	"input_text",
	"other",
	"output_audio",
	"output_image",
	"output_text",
	"reasoning",
	"server_tool_use",
}

var tokenNativeUnitKeys = []string{
	"input_audio_seconds",
	"input_image_pixels",
	"input_images",
	"output_audio_seconds",
	"output_image_pixels",
	"output_images",
}

func zeroBuckets() map[string]uint64 {
	m := make(map[string]uint64, len(tokenBucketKeys))
	for _, k := range tokenBucketKeys {
		m[k] = 0
	}
	return m
}

func zeroNativeUnits() map[string]float64 {
	m := make(map[string]float64, len(tokenNativeUnitKeys))
	for _, k := range tokenNativeUnitKeys {
		m[k] = 0
	}
	return m
}

func copyBuckets(m map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyNativeUnits(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type BucketValue struct {
	Key   string
	Value uint64
}

type NativeUnitValue struct {
	Key   string
	Value float64
}

type TokenAccounting struct {
	EndpointKind     string
	InputTokens      uint32
	OutputTokens     uint32
	CacheReadTokens  uint32
	CacheWriteTokens uint32
	TokenUsageJSON   string
	buckets          map[string]uint64
	nativeUnits      map[string]float64
}

func NewTokenAccounting(endpointKind string) TokenAccounting {
	return TokenAccounting{
		EndpointKind: endpointKind,
		buckets:      zeroBuckets(),
		nativeUnits:  zeroNativeUnits(),
	}
}

func (t TokenAccounting) WithBuckets(values ...BucketValue) TokenAccounting {
	for _, v := range values {
		if _, ok := t.buckets[v.Key]; ok {
			t.buckets[v.Key] = v.Value
		}
	}
	return t
}

func (t TokenAccounting) WithNativeUnits(values ...NativeUnitValue) TokenAccounting {
	for _, v := range values {
		if _, ok := t.nativeUnits[v.Key]; ok {
			t.nativeUnits[v.Key] = v.Value
		}
	}
	return t
}

func (t TokenAccounting) WithLegacyScalars(input, output, cacheRead, cacheWrite uint64) TokenAccounting {
	t.InputTokens = capU32(input)
	t.OutputTokens = capU32(output)
	t.CacheReadTokens = capU32(cacheRead)
	t.CacheWriteTokens = capU32(cacheWrite)
	return t
}

func (t TokenAccounting) WithRawUsageJSON(s string) TokenAccounting {
	t.TokenUsageJSON = s
	return t
}

func (t TokenAccounting) WithRawUsage(usage JSON) TokenAccounting {
	if IsNull(usage) {
		return t
	}
	t.TokenUsageJSON = Compact(usage)
	return t
}

// FirstOf returns the first member of v present under any of keys, explicit
// null included.
func FirstOf(v JSON, keys ...string) JSON {
	m, ok := AsObject(v)
	if !ok {
		return nil
	}
	for _, key := range keys {
		if val, present := m[key]; present {
			return val
		}
	}
	return nil
}

func (t TokenAccounting) Buckets() map[string]uint64 { return copyBuckets(t.buckets) }

func (t TokenAccounting) NativeUnits() map[string]float64 { return copyNativeUnits(t.nativeUnits) }

func capU32(v uint64) uint32 {
	return uint32(min(v, uint64(^uint32(0))))
}

func SatSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

func usageFallback(usage JSON, keys []string, details JSON, detailKey string) uint64 {
	if v := FirstOf(usage, keys...); v != nil {
		return ToU64(v)
	}
	return ToU64(Get(details, detailKey))
}

func OpenAIGeneration(usage JSON) TokenAccounting {
	t := NewTokenAccounting("generation")
	if IsNull(usage) {
		return t
	}
	inputTotal := ToU64(FirstOf(usage, "input_tokens", "prompt_tokens", "total_input_tokens"))
	outputTotal := ToU64(FirstOf(usage, "output_tokens", "completion_tokens", "total_output_tokens"))
	inputDetails := FirstOf(usage, "input_tokens_details", "prompt_tokens_details")
	outputDetails := FirstOf(usage, "output_tokens_details", "completion_tokens_details")

	cacheRead := usageFallback(usage, []string{"cached_input_tokens", "cache_read_input_tokens"}, inputDetails, "cached_tokens")
	cacheWrite := usageFallback(usage, []string{"cache_creation_input_tokens", "cache_write_input_tokens"}, inputDetails, "cache_creation_tokens")
	inputImage := ToU64(Get(inputDetails, "image_tokens"))
	inputAudio := ToU64(Get(inputDetails, "audio_tokens"))
	outputImage := ToU64(Get(outputDetails, "image_tokens"))
	outputAudio := ToU64(Get(outputDetails, "audio_tokens"))
	reasoning := ToU64(Get(outputDetails, "reasoning_tokens"))
	serverToolUse := SumNumericObject(Get(usage, "server_tool_use"))

	inputTextDetail := ToU64(Get(inputDetails, "text_tokens"))
	outputTextDetail := ToU64(Get(outputDetails, "text_tokens"))
	inputText := inputTextDetail
	if inputText == 0 {
		inputText = SatSub(inputTotal, cacheRead+cacheWrite+inputImage+inputAudio)
	}
	outputText := outputTextDetail
	if outputText == 0 {
		outputText = SatSub(outputTotal, outputImage+outputAudio+reasoning+serverToolUse)
	}

	return t.WithBuckets(
		BucketValue{Key: "input_text", Value: inputText},
		BucketValue{Key: "output_text", Value: outputText},
		BucketValue{Key: "input_cache_read", Value: cacheRead},
		BucketValue{Key: "input_cache_write", Value: cacheWrite},
		BucketValue{Key: "input_image", Value: inputImage},
		BucketValue{Key: "output_image", Value: outputImage},
		BucketValue{Key: "input_audio", Value: inputAudio},
		BucketValue{Key: "output_audio", Value: outputAudio},
		BucketValue{Key: "reasoning", Value: reasoning},
		BucketValue{Key: "server_tool_use", Value: serverToolUse},
	).WithLegacyScalars(inputTotal, outputTotal, cacheRead, cacheWrite).
		WithRawUsage(usage)
}

func AsObjectOrEmpty(v JSON) map[string]any {
	m, _ := AsObject(v)
	return m
}
