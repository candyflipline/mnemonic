package shared

// Token bucket and native-unit key sets, in the order the golden maps carry
// them (alphabetical, since the maps serialize sorted).
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

// BucketValue is one named bucket entry.
type BucketValue struct {
	Key   string
	Value uint64
}

// NativeUnitValue is one named native-unit entry.
type NativeUnitValue struct {
	Key   string
	Value float64
}

// TokenAccounting mirrors TokenAccounting: per-event token totals plus the
// harmonized bucket and native-unit maps.
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

// NewTokenAccounting starts a zero-valued accounting for an endpoint kind.
func NewTokenAccounting(endpointKind string) TokenAccounting {
	return TokenAccounting{
		EndpointKind: endpointKind,
		buckets:      zeroBuckets(),
		nativeUnits:  zeroNativeUnits(),
	}
}

// WithBuckets sets named buckets, ignoring names outside the key set.
func (t TokenAccounting) WithBuckets(values ...BucketValue) TokenAccounting {
	for _, v := range values {
		if _, ok := t.buckets[v.Key]; ok {
			t.buckets[v.Key] = v.Value
		}
	}
	return t
}

// WithNativeUnits sets named native units, ignoring names outside the key set.
func (t TokenAccounting) WithNativeUnits(values ...NativeUnitValue) TokenAccounting {
	for _, v := range values {
		if _, ok := t.nativeUnits[v.Key]; ok {
			t.nativeUnits[v.Key] = v.Value
		}
	}
	return t
}

// WithLegacyScalars sets the flat token counters.
func (t TokenAccounting) WithLegacyScalars(input, output, cacheRead, cacheWrite uint64) TokenAccounting {
	t.InputTokens = capU32(input)
	t.OutputTokens = capU32(output)
	t.CacheReadTokens = capU32(cacheRead)
	t.CacheWriteTokens = capU32(cacheWrite)
	return t
}

// WithRawUsageJSON records usage JSON verbatim on the row.
func (t TokenAccounting) WithRawUsageJSON(s string) TokenAccounting {
	t.TokenUsageJSON = s
	return t
}

// WithRawUsage records the compact JSON of a usage object on the row, like
// with_raw_usage.
func (t TokenAccounting) WithRawUsage(usage JSON) TokenAccounting {
	if IsNull(usage) {
		return t
	}
	t.TokenUsageJSON = Compact(usage)
	return t
}

// FirstOf returns the first member of v present under any of keys, explicit
// null included, mirroring Rust's Option::or_else chains over Value::get.
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

// Buckets returns a private copy of the bucket map.
func (t TokenAccounting) Buckets() map[string]uint64 { return copyBuckets(t.buckets) }

// NativeUnits returns a private copy of the native-unit map.
func (t TokenAccounting) NativeUnits() map[string]float64 { return copyNativeUnits(t.nativeUnits) }

func capU32(v uint64) uint32 {
	return uint32(min(v, uint64(^uint32(0))))
}

// SatSub is u64 saturating_sub, which the upstream token arithmetic leans on.
func SatSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// usageFallback resolves the first present usage member, explicit null
// included, and otherwise falls back to an input-details subkey.
func usageFallback(usage JSON, keys []string, details JSON, detailKey string) uint64 {
	if v := FirstOf(usage, keys...); v != nil {
		return ToU64(v)
	}
	return ToU64(Get(details, detailKey))
}

// OpenAIGeneration mirrors TokenAccounting::openai_generation for OpenAI-style
// usage objects.
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

// AsObjectOrEmpty returns the object form of v, or an empty map.
func AsObjectOrEmpty(v JSON) map[string]any {
	m, _ := AsObject(v)
	return m
}
