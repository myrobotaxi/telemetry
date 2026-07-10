package commands

import "fmt"

// Param helpers extract and validate typed values from the decoded JSON
// request body (map[string]any, so numbers arrive as float64). Each
// returns a typed *CommandError (invalid_request) on a type or presence
// violation, so BuildBody functions stay small and uniform.

func paramFloat(p map[string]any, key string, required bool) (value float64, present bool, err error) {
	v, ok := p[key]
	if !ok {
		if required {
			return 0, false, errInvalidParams(fmt.Sprintf("missing required parameter %q", key))
		}
		return 0, false, nil
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false, errInvalidParams(fmt.Sprintf("parameter %q must be a number", key))
	}
	return f, true, nil
}

func paramInt(p map[string]any, key string, required bool) (value int, present bool, err error) {
	f, present, err := paramFloat(p, key, required)
	if err != nil || !present {
		return 0, present, err
	}
	if f != float64(int(f)) {
		return 0, false, errInvalidParams(fmt.Sprintf("parameter %q must be an integer", key))
	}
	return int(f), true, nil
}

// paramString requires the key: every current caller treats a string
// parameter as mandatory, so it returns just (value, error).
func paramString(p map[string]any, key string) (string, error) {
	v, ok := p[key]
	if !ok {
		return "", errInvalidParams(fmt.Sprintf("missing required parameter %q", key))
	}
	s, ok := v.(string)
	if !ok {
		return "", errInvalidParams(fmt.Sprintf("parameter %q must be a string", key))
	}
	return s, nil
}
