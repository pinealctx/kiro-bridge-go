package sanitizer

import (
	"encoding/json"
	"fmt"
	"log"
)

// builtinToClientMap maps Kiro builtin tool names to candidate client tool names (ordered by preference).
var builtinToClientMap = map[string][]string{
	"readFile":         {"Read"},
	"readCode":         {"Read"},
	"fs_read":          {"Read"},
	"fsWrite":          {"Write"},
	"fs_write":         {"Write"},
	"executeCommand":   {"Bash"},
	"listDirectory":    {"Bash"},
	"searchFiles":      {"Bash"},
	"grepSearch":       {"Bash"},
	"webSearch":        {"WebSearch"},
	"web_search":       {"WebSearch"},
	"websearch":        {"WebSearch"},
	"fetchWebpage":     {"WebFetch"},
	"getDefinition":    {"LSP"},
	"getReferences":    {"LSP"},
	"getTypeDefinition": {"LSP"},
	"getDiagnostics":   {"LSP"},
	"smartRelocate":    {},
	"browser_navigate": {},
	"browser_snapshot": {},
	"browser_click":    {},
	"browser_type":     {},
}

// RemapBuiltinTool tries to remap a Kiro builtin tool call to a matching client tool.
// Returns (clientToolName, transformedInput, true) on success, or ("", nil, false) if no mapping.
func RemapBuiltinTool(kiroName string, kiroInput interface{}, clientToolNames map[string]bool) (string, interface{}, bool) {
	candidates, known := builtinToClientMap[kiroName]
	if !known || len(candidates) == 0 {
		return "", nil, false
	}

	for _, name := range candidates {
		if clientToolNames[name] {
			transformed := transformInput(kiroName, kiroInput, name)
			return name, transformed, true
		}
	}
	return "", nil, false
}

// ClientToolNameSet builds a set of tool names from the client's tool list (OpenAI format).
func ClientToolNameSet(tools []map[string]interface{}) map[string]bool {
	names := make(map[string]bool, len(tools))
	for _, t := range tools {
		if fn, ok := t["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok {
				names[name] = true
			}
		}
	}
	return names
}

func transformInput(kiroName string, kiroInput interface{}, _ string) interface{} {
	inputMap, ok := toMap(kiroInput)
	if !ok {
		log.Printf("[remap] cannot parse input for %s, passing through as-is", kiroName)
		return kiroInput
	}

	switch kiroName {
	case "readFile", "readCode", "fs_read":
		return map[string]interface{}{"file_path": extractPath(inputMap)}

	case "fsWrite", "fs_write":
		result := map[string]interface{}{"file_path": extractPath(inputMap)}
		if c, ok := inputMap["content"].(string); ok {
			result["content"] = c
		} else if c, ok := inputMap["newContent"].(string); ok {
			result["content"] = c
		}
		return result

	case "executeCommand":
		cmd, _ := inputMap["command"].(string)
		return map[string]interface{}{"command": cmd}

	case "listDirectory":
		p := extractPath(inputMap)
		if p == "" {
			p = "."
		}
		return map[string]interface{}{"command": fmt.Sprintf("ls -la %q", p)}

	case "searchFiles":
		query, _ := inputMap["query"].(string)
		if query == "" {
			query, _ = inputMap["pattern"].(string)
		}
		p := extractPath(inputMap)
		if p == "" {
			p = "."
		}
		return map[string]interface{}{"command": fmt.Sprintf("find %q -name %q", p, "*"+query+"*")}

	case "grepSearch":
		query, _ := inputMap["query"].(string)
		if query == "" {
			query, _ = inputMap["pattern"].(string)
		}
		p := extractPath(inputMap)
		if p == "" {
			p = "."
		}
		return map[string]interface{}{"command": fmt.Sprintf("grep -rn %q %q", query, p)}

	case "webSearch", "web_search", "websearch":
		query, _ := inputMap["query"].(string)
		return map[string]interface{}{"query": query}

	case "fetchWebpage":
		url, _ := inputMap["url"].(string)
		return map[string]interface{}{"url": url, "prompt": "Extract the main content from this page."}

	case "getDefinition", "getReferences", "getTypeDefinition":
		opMap := map[string]string{
			"getDefinition":     "goToDefinition",
			"getReferences":     "findReferences",
			"getTypeDefinition": "goToDefinition",
		}
		result := map[string]interface{}{
			"operation": opMap[kiroName],
			"filePath":  extractPath(inputMap),
		}
		if line, ok := inputMap["line"]; ok {
			result["line"] = line
		}
		if ch, ok := inputMap["character"]; ok {
			result["character"] = ch
		}
		return result

	case "getDiagnostics":
		return map[string]interface{}{
			"operation": "documentSymbol",
			"filePath":  extractPath(inputMap),
			"line":      1,
			"character": 1,
		}
	}

	return kiroInput
}

func extractPath(m map[string]interface{}) string {
	for _, key := range []string{"relativePath", "path", "filePath", "file_path", "fileName"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func toMap(v interface{}) (map[string]interface{}, bool) {
	if m, ok := v.(map[string]interface{}); ok {
		return m, true
	}
	if s, ok := v.(string); ok && s != "" {
		var m map[string]interface{}
		if json.Unmarshal([]byte(s), &m) == nil {
			return m, true
		}
	}
	return nil, false
}
