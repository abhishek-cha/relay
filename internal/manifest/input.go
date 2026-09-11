package manifest

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"relay/pkg/relay"
)

// ValidateInput checks an input object against an operation's declared schema
// and returns a structured error describing the first violation.
//
// Both the tool runtime and the daemon call this. The tool validates so a user
// gets an immediate local error; the daemon validates again because a tool
// binary is treated as potentially untrusted and the trusted component cannot
// assume its caller did the work (spec §40).
func (t *Tool) ValidateInput(input map[string]any) *relay.Error {
	for _, name := range t.Input.Required {
		value, present := input[name]
		if !present || value == nil {
			return relay.NewError(relay.CodeInvalidInput,
				fmt.Sprintf("missing required input %q", name))
		}
	}

	names := make([]string, 0, len(input))
	for name := range input {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		value := input[name]
		if value == nil {
			continue // an explicit null is treated as absent
		}
		property, declared := t.Input.Properties[name]
		if !declared {
			return relay.NewError(relay.CodeInvalidInput,
				fmt.Sprintf("unknown input %q", name)).
				WithDetails(map[string]any{"accepted": t.inputNames()})
		}
		if err := checkType(name, property, value); err != nil {
			return err
		}
		if err := checkEnum(name, property, value); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tool) inputNames() []string {
	names := make([]string, 0, len(t.Input.Properties))
	for name := range t.Input.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func checkType(name string, property Property, value any) *relay.Error {
	describe := func(want string) *relay.Error {
		return relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("input %q must be %s", name, want))
	}
	switch property.Type {
	case "string":
		if _, ok := value.(string); !ok {
			return describe("a string")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return describe("a boolean")
		}
	case "integer":
		number, ok := asNumber(value)
		if !ok || number != math.Trunc(number) {
			return describe("an integer")
		}
	case "number":
		if _, ok := asNumber(value); !ok {
			return describe("a number")
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return describe("an object")
		}
	case "array":
		if !isSlice(value) {
			return describe("an array")
		}
	}
	return nil
}

// isSlice reports whether value is a slice, whatever its element type.
//
// An array reaches this package in one of two Go shapes: the daemon decodes
// JSON into []any, while the tool runtime's flag layer builds typed slices
// such as []string and []int64 (spec §11). Both are arrays, so the check is
// structural instead of a match on the exact dynamic type.
func isSlice(value any) bool {
	kind := reflect.TypeOf(value)
	return kind != nil && kind.Kind() == reflect.Slice
}

func checkEnum(name string, property Property, value any) *relay.Error {
	if len(property.Enum) == 0 {
		return nil
	}
	for _, allowed := range property.Enum {
		if equalValue(allowed, value) {
			return nil
		}
	}
	options := make([]string, 0, len(property.Enum))
	for _, allowed := range property.Enum {
		options = append(options, fmt.Sprint(allowed))
	}
	return relay.NewError(relay.CodeInvalidInput,
		fmt.Sprintf("input %q must be one of: %s", name, strings.Join(options, ", ")))
}

// asNumber coerces the numeric representations a YAML manifest or a JSON
// payload can produce.
func asNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint64:
		return float64(number), true
	default:
		return 0, false
	}
}

// equalValue compares an enum entry against a supplied value, tolerating the
// int-versus-float difference between a YAML manifest and a JSON request.
func equalValue(allowed, value any) bool {
	allowedNumber, allowedOK := asNumber(allowed)
	valueNumber, valueOK := asNumber(value)
	if allowedOK && valueOK {
		return allowedNumber == valueNumber
	}
	return fmt.Sprint(allowed) == fmt.Sprint(value)
}
