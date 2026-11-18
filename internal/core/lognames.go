package core

import "strings"

// Names for the log. A script, application or API command that a dialplan variable or a hook names can be any text
// its author wrote, and a template can pull any channel variable into that text. So the log carries only names the
// switch itself knows: the returned string is the registry's own key (found by comparing, not copied from the text),
// or "(unknown)".

func knownScript(name string) string {
	n := scriptName(name)
	scriptMu.RLock()
	defer scriptMu.RUnlock()
	for k := range scriptMap {
		if k == n {
			return k
		}
	}
	return "(unknown)"
}

func (c *Core) knownApp(name string) string {
	for k := range c.apps {
		if k == name {
			return k
		}
	}
	return "(unknown)"
}

func knownAPI(name string) string {
	for k := range apiCommands {
		if k == name {
			return k
		}
	}
	return "(unknown)"
}

// hookLabel says which api_hangup_hook ran, from its template, never from the expanded text.
func hookLabel(tmpl string) string {
	f := strings.Fields(tmpl)
	switch {
	case len(f) >= 2 && (f[0] == "lua" || f[0] == "luarun"):
		return "lua " + knownScript(f[1])
	case len(f) >= 1:
		return "api " + knownAPI(f[0])
	}
	return ""
}
