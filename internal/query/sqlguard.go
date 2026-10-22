package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
)

// sqlViews are the only relations a query statement may name.
var sqlViews = []string{"annotations", "events", "sessions", "spans", "turns"}

// sqlGuard takes one statement from the caller and returns the statement to
// run. DuckDB's own parser serializes it (json_serialize_sql, which refuses
// everything but a SELECT), checkStatement walks that tree, and the run
// statement is the tree printed back (json_deserialize_sql), parsed and
// walked again, so what runs is exactly what was checked.
func sqlGuard(ctx context.Context, c *sql.Conn, stmt string) (string, error) {
	if strings.ContainsRune(stmt, 0) {
		return "", argErrorf("sql must not contain a NUL byte")
	}
	ast, err := serializeSQL(ctx, c, stmt)
	if err != nil {
		return "", err
	}
	if err := checkStatement(ast); err != nil {
		return "", err
	}
	var printed string
	if err := c.QueryRowContext(ctx, "SELECT json_deserialize_sql(json("+sqlQuote(string(ast))+"))::VARCHAR").Scan(&printed); err != nil {
		return "", argErrorf("sql cannot be rewritten: %s", err)
	}
	ast, err = serializeSQL(ctx, c, printed)
	if err != nil {
		return "", err
	}
	if err := checkStatement(ast); err != nil {
		return "", err
	}
	return printed, nil
}

func serializeSQL(ctx context.Context, c *sql.Conn, stmt string) ([]byte, error) {
	var out string
	if err := c.QueryRowContext(ctx, "SELECT json_serialize_sql("+sqlQuote(stmt)+")::VARCHAR").Scan(&out); err != nil {
		return nil, err
	}
	var r struct {
		Error        bool   `json:"error"`
		ErrorMessage string `json:"error_message"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return nil, err
	}
	if r.Error {
		if strings.HasPrefix(r.ErrorMessage, "Only SELECT statements") {
			return nil, argErrorf("only one SELECT statement is allowed")
		}
		return nil, argErrorf("invalid sql: %s", r.ErrorMessage)
	}
	return []byte(out), nil
}

type astObj = map[string]any

// checkStatement refuses a serialized statement unless it is one SELECT
// over the sqlViews (and CTEs defined in it) that calls only allowed
// functions. Everything not named here is refused: an unknown query node, an
// unknown table expression, a function off the list.
func checkStatement(ast []byte) error {
	var doc struct {
		Statements []astObj `json:"statements"`
	}
	if err := json.Unmarshal(ast, &doc); err != nil {
		return err
	}
	if len(doc.Statements) != 1 {
		return argErrorf("exactly one statement is allowed, got %d", len(doc.Statements))
	}
	return checkNode(doc.Statements[0]["node"], nil)
}

func checkNode(v any, scope []string) error {
	n, _ := v.(astObj)
	switch n["type"] {
	case "SELECT_NODE", "SET_OPERATION_NODE":
	case "RECURSIVE_CTE_NODE":
		return argErrorf("recursive CTEs are not allowed")
	default:
		return argErrorf("query node %v is not allowed", n["type"])
	}
	cte, _ := n["cte_map"].(astObj)
	entries, _ := cte["map"].([]any)
	for _, e := range entries {
		e, _ := e.(astObj)
		value, _ := e["value"].(astObj)
		query, _ := value["query"].(astObj)
		if err := checkNode(query["node"], scope); err != nil {
			return err
		}
		name, _ := e["key"].(string)
		scope = append(scope[:len(scope):len(scope)], strings.ToLower(name))
	}
	for k, child := range n {
		if k == "cte_map" {
			continue
		}
		var err error
		if k == "from_table" {
			err = checkTable(child, scope)
		} else {
			err = checkExpr(child, scope)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func checkTable(v any, scope []string) error {
	t, _ := v.(astObj)
	switch t["type"] {
	case "EMPTY":
		return nil
	case "BASE_TABLE":
		return checkBaseTable(t, scope)
	case "SUBQUERY":
		sub, _ := t["subquery"].(astObj)
		return checkNode(sub["node"], scope)
	case "JOIN":
		for _, k := range []string{"left", "right"} {
			if err := checkTable(t[k], scope); err != nil {
				return err
			}
		}
		return checkExpr(t["condition"], scope)
	case "EXPRESSION_LIST":
		return checkExpr(t["values"], scope)
	case "TABLE_FUNCTION":
		f, _ := t["function"].(astObj)
		return argErrorf("table function %q is not allowed", f["function_name"])
	}
	return argErrorf("table expression %v is not allowed", t["type"])
}

func checkBaseTable(t astObj, scope []string) error {
	catalog, _ := t["catalog_name"].(string)
	schema, _ := t["schema_name"].(string)
	name, _ := t["table_name"].(string)
	if catalog != "" || schema != "" {
		return argErrorf("table %q must be named without a schema; query one of %s", join(catalog, schema, name), strings.Join(sqlViews, ", "))
	}
	lower := strings.ToLower(name)
	if !slices.Contains(sqlViews, lower) && !slices.Contains(scope, lower) {
		return argErrorf("table %q is not queryable; query one of %s", name, strings.Join(sqlViews, ", "))
	}
	return nil
}

func join(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ".")
}

var tableOnly = []string{"BASE_TABLE", "TABLE_FUNCTION", "SHOW_REF", "JOIN", "EMPTY", "EXPRESSION_LIST", "PIVOT", "COLUMN_DATA", "CTE"}

// checkExpr walks any value that is not a table expression: it checks every
// function call, descends into subqueries, and refuses a table expression
// found where none belongs.
func checkExpr(v any, scope []string) error {
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			if err := checkExpr(e, scope); err != nil {
				return err
			}
		}
	case astObj:
		if t, _ := v["type"].(string); t == "SELECT_NODE" || t == "SET_OPERATION_NODE" || t == "RECURSIVE_CTE_NODE" {
			return checkNode(v, scope)
		} else if _, hasClass := v["class"]; !hasClass && slices.Contains(tableOnly, t) {
			return argErrorf("table expression %s is not allowed here", t)
		}
		if v["class"] == "PARAMETER" {
			return argErrorf("query parameters are not allowed; write the value into sql")
		}
		// The parser itself emits main.list_value for [1, 2] and the like.
		if name, ok := v["function_name"].(string); ok {
			schema, _ := v["schema"].(string)
			catalog, _ := v["catalog"].(string)
			if (schema != "" && schema != "main") || catalog != "" || !slices.Contains(sqlFunctions, strings.ToLower(name)) {
				return argErrorf("function %q is not allowed", join(catalog, schema, name))
			}
		}
		for _, child := range v {
			if err := checkExpr(child, scope); err != nil {
				return err
			}
		}
	}
	return nil
}

// sqlFunctions is every function a query may call: operators, aggregates
// and window functions, and pure scalar functions over strings, numbers,
// dates and times, JSON, lists, maps and structs. Nothing that reads the
// environment, the filesystem or settings, changes state, or builds a large
// value from a small argument (repeat, range, lpad, printf).
var sqlFunctions = []string{
	"!~~", "!~~*", "%", "&", "&&", "*", "**", "+", "-", "->>", "/", "//", "<->", "<<", "<=>", "<@", ">>", "@", "@>",
	"^", "^@", "|", "||", "~", "~~", "~~*", "~~~",
	"abs", "acos", "acosh", "add", "age", "any_value", "approx_count_distinct", "approx_quantile", "approx_top_k",
	"arbitrary", "arg_max", "arg_max_null", "arg_max_nulls_last", "arg_min", "arg_min_null", "arg_min_nulls_last",
	"argmax", "argmin", "array_agg", "array_aggr", "array_aggregate", "array_append", "array_apply", "array_cat",
	"array_concat", "array_contains", "array_distinct", "array_extract", "array_filter", "array_has", "array_has_all",
	"array_has_any", "array_indexof", "array_intersect", "array_length", "array_pop_back", "array_pop_front",
	"array_position", "array_prepend", "array_push_back", "array_push_front", "array_reduce", "array_reverse",
	"array_reverse_sort", "array_select", "array_slice", "array_sort", "array_to_json", "array_to_string",
	"array_transform", "array_unique", "array_value", "array_where", "array_zip", "ascii", "asin", "asinh", "atan",
	"atan2", "atanh", "avg", "base64", "bin", "bit_and", "bit_count", "bit_length", "bit_or", "bit_xor", "bool_and",
	"bool_or", "cardinality", "cbrt", "ceil", "ceiling", "century", "char_length", "character_length", "chr",
	"concat", "concat_ws", "contains", "corr", "cos", "cosh", "cot", "count", "count_if", "count_star",
	"countif", "covar_pop", "covar_samp", "cume_dist", "current_date", "current_localtime", "current_localtimestamp",
	"damerau_levenshtein", "date_add", "date_diff", "date_part", "date_sub", "date_trunc", "datediff", "datepart",
	"datesub", "datetrunc", "day", "dayname", "dayofmonth", "dayofweek", "dayofyear", "decade", "degrees",
	"dense_rank", "divide", "editdist3", "element_at", "ends_with", "entropy", "epoch", "epoch_ms", "epoch_ns",
	"epoch_us", "era", "even", "exp", "factorial", "favg", "fdiv", "filter", "first", "first_value", "flatten",
	"floor", "fmod", "format_bytes", "formatReadableDecimalSize", "formatReadableSize", "from_base64", "from_hex",
	"from_json", "from_json_strict", "fsum", "gamma", "gcd", "geomean", "geometric_mean", "get_current_time",
	"get_current_timestamp", "greatest", "greatest_common_divisor", "group_concat", "hamming", "hash", "hex",
	"histogram", "histogram_exact", "hour", "ilike_escape", "instr", "isfinite", "isinf", "isnan", "isodow",
	"isoyear", "jaccard", "jaro_similarity", "jaro_winkler_similarity", "json", "json_array", "json_array_length",
	"json_contains", "json_exists", "json_extract", "json_extract_path", "json_extract_path_text",
	"json_extract_string", "json_group_array", "json_group_object", "json_group_structure", "json_keys",
	"json_merge_patch", "json_object", "json_pretty", "json_quote", "json_structure", "json_transform",
	"json_transform_strict", "json_type", "json_valid", "json_value", "julian", "kahan_sum", "kurtosis",
	"kurtosis_pop", "lag", "last", "last_day", "last_value", "lcase", "lcm", "lead", "least", "least_common_multiple",
	"left", "left_grapheme", "len", "length", "length_grapheme", "levenshtein", "lgamma", "like_escape", "list",
	"list_aggr", "list_aggregate", "list_any_value", "list_append", "list_apply", "list_approx_count_distinct",
	"list_avg", "list_bit_and", "list_bit_or", "list_bit_xor", "list_bool_and", "list_bool_or", "list_cat",
	"list_concat", "list_contains", "list_count", "list_distinct", "list_element", "list_entropy", "list_extract",
	"list_filter", "list_first", "list_has", "list_has_all", "list_has_any", "list_histogram", "list_indexof",
	"list_intersect", "list_kurtosis", "list_kurtosis_pop", "list_last", "list_mad", "list_max", "list_median",
	"list_min", "list_mode", "list_pack", "list_position", "list_prepend", "list_product", "list_reduce",
	"list_reverse", "list_reverse_sort", "list_select", "list_sem", "list_skewness", "list_slice", "list_sort",
	"list_stddev_pop", "list_stddev_samp", "list_string_agg", "list_sum", "list_transform", "list_unique",
	"list_value", "list_var_pop", "list_var_samp", "list_where", "list_zip", "listagg", "ln", "log", "log10", "log2",
	"lower", "ltrim", "mad", "make_date", "make_time", "make_timestamp", "make_timestamp_ms", "make_timestamp_ns",
	"make_timestamptz", "map", "map_concat", "map_contains", "map_contains_entry", "map_contains_value",
	"map_entries", "map_extract", "map_extract_value", "map_from_entries", "map_keys", "map_values", "max", "max_by",
	"md5", "md5_number", "md5_number_lower", "md5_number_upper", "mean", "median", "microsecond", "millennium",
	"millisecond", "min", "min_by", "minute", "mismatches", "mod", "mode", "month", "monthname", "multiply",
	"nanosecond", "nextafter", "nfc_normalize", "now", "nth_value", "ntile", "nullif", "octet_length", "ord",
	"percent_rank", "pi", "position", "pow", "power", "prefix", "product", "quantile", "quantile_cont",
	"quantile_disc", "quarter", "radians", "rank", "rank_dense", "regexp_escape", "regexp_extract",
	"regexp_extract_all", "regexp_full_match", "regexp_matches", "regexp_replace", "regexp_split_to_array",
	"regr_avgx", "regr_avgy", "regr_count", "regr_intercept", "regr_r2", "regr_slope", "regr_sxx", "regr_sxy",
	"regr_syy", "replace", "reservoir_quantile", "reverse", "right", "right_grapheme", "round", "round_even",
	"roundbankers", "row", "row_number", "row_to_json", "rtrim", "sem", "second", "sha1", "sha256", "sign",
	"signbit", "sin", "sinh", "skewness", "split", "split_part", "sqrt", "starts_with", "stddev", "stddev_pop",
	"stddev_samp", "str_split", "str_split_regex", "string_agg", "string_split", "string_split_regex",
	"string_to_array", "strip_accents", "strlen", "strftime", "strpos", "strptime", "struct_concat",
	"struct_contains", "struct_extract", "struct_extract_at", "struct_has", "struct_indexof", "struct_insert",
	"struct_keys", "struct_pack", "struct_position", "struct_update", "struct_values", "substr", "substring",
	"substring_grapheme", "subtract", "suffix", "sum", "sum_no_overflow", "sumkahan", "tan", "tanh", "time_bucket",
	"timezone", "timezone_hour", "timezone_minute", "to_base64", "to_binary", "to_centuries", "to_days",
	"to_decades", "to_hex", "to_hours", "to_json", "to_microseconds", "to_millennia", "to_milliseconds",
	"to_minutes", "to_months", "to_quarters", "to_seconds", "to_timestamp", "to_weeks", "to_years", "today",
	"transaction_timestamp", "translate", "trim", "trunc", "try_strptime", "typeof", "ucase", "unbin", "unhex",
	"unicode", "unnest", "upper", "url_decode", "url_encode", "var_pop", "var_samp", "variance", "wavg",
	"week", "weekday", "weekofyear", "weighted_avg", "xor", "year", "yearweek",
}
