package runtime

import (
	"flag"
	"os"
	"strconv"
	"strings"

	"relay/internal/manifest"
)

// flagValue adapts a manifest input property to a command-line flag. Arguments
// are generated from the input schema (spec §11).
type flagValue struct {
	kind   string
	text   string
	number float64
	count  int64
	truth  bool
	items  []string
}

func newFlagValue(flags *flag.FlagSet, name string, property manifest.Property) *flagValue {
	value := &flagValue{kind: strings.ToLower(property.Type)}
	usage := property.Description

	switch value.kind {
	case "", "string":
		value.kind = "string"
		flags.StringVar(&value.text, name, "", usage)
	case "integer":
		flags.Int64Var(&value.count, name, 0, usage)
	case "number":
		flags.Float64Var(&value.number, name, 0, usage)
	case "boolean":
		flags.BoolVar(&value.truth, name, false, usage)
	case "array":
		itemType := "string"
		if property.Items != nil && property.Items.Type != "" {
			itemType = property.Items.Type
		}
		value.kind = "array:" + itemType
		flags.Var(&arrayValue{target: &value.items}, name, usage+" (repeatable)")
	default:
		// Objects and unknown types are supplied through --input or
		// --input-json rather than synthesised into flags.
		return nil
	}
	return value
}

func (v *flagValue) get() any {
	switch v.kind {
	case "string":
		return v.text
	case "integer":
		return v.count
	case "number":
		return v.number
	case "boolean":
		return v.truth
	}
	if !strings.HasPrefix(v.kind, "array:") {
		return nil
	}
	switch strings.TrimPrefix(v.kind, "array:") {
	case "integer":
		numbers := make([]int64, 0, len(v.items))
		for _, item := range v.items {
			parsed, err := strconv.ParseInt(item, 10, 64)
			if err != nil {
				return v.items
			}
			numbers = append(numbers, parsed)
		}
		return numbers
	case "number":
		numbers := make([]float64, 0, len(v.items))
		for _, item := range v.items {
			parsed, err := strconv.ParseFloat(item, 64)
			if err != nil {
				return v.items
			}
			numbers = append(numbers, parsed)
		}
		return numbers
	default:
		return v.items
	}
}

// arrayValue collects a repeatable flag.
type arrayValue struct {
	target *[]string
}

func (a *arrayValue) String() string {
	if a.target == nil {
		return ""
	}
	return strings.Join(*a.target, ",")
}

func (a *arrayValue) Set(item string) error {
	*a.target = append(*a.target, item)
	return nil
}

// readFile is a seam kept tiny so tests can substitute their own reads.
var readFile = os.ReadFile
