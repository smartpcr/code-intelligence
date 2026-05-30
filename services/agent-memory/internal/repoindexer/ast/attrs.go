package ast

// MergeLangMeta merges parser-supplied LangMeta into attrs_json, protecting
// first-class keys that the dispatcher owns. The dispatcher's language value
// always takes precedence over anything the parser places in LangMeta.
//
// First-class keys: "language".
func MergeLangMeta(parserMeta map[string]string, dispatcherLanguage string) map[string]string {
	result := make(map[string]string, len(parserMeta)+1)
	for k, v := range parserMeta {
		result[k] = v
	}
	// First-class key: dispatcher wins regardless of parser-supplied value.
	result["language"] = dispatcherLanguage
	return result
}
