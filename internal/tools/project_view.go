package tools

// Project outputs are default-deny: only these keys leave the server, so
// secrets such as runners_token (and any field the SDK adds later) cannot leak.
var (
	projectFields = fieldSet("id", "name", "name_with_namespace", "path", "path_with_namespace",
		"description", "default_branch", "visibility", "web_url", "http_url_to_repo", "ssh_url_to_repo",
		"readme_url", "topics", "archived", "empty_repo", "created_at", "updated_at", "last_activity_at",
		"namespace", "forked_from_project")
	projectNamespaceFields = fieldSet("id", "name", "path", "kind", "full_path", "web_url")
	projectForkFields      = fieldSet("id", "name", "name_with_namespace", "path", "path_with_namespace",
		"http_url_to_repo", "web_url")
)

func fieldSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// projectView returns a JSON tree of one project, or of a slice of projects,
// reduced to the allowlisted fields.
func projectView(v any) any {
	switch t := Out(v).(type) {
	case []any:
		for i, p := range t {
			t[i] = pick(p, projectFields)
		}
		return t
	default:
		return pick(t, projectFields)
	}
}

// pick keeps the allowed keys of a JSON object; nested namespace and fork
// objects are reduced with their own allowlists.
func pick(v any, allowed map[string]bool) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(allowed))
	for k, x := range m {
		if !allowed[k] {
			continue
		}
		switch k {
		case "namespace":
			x = pick(x, projectNamespaceFields)
		case "forked_from_project":
			x = pick(x, projectForkFields)
		}
		out[k] = x
	}
	return out
}
