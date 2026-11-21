// Prompt templates (backlog F8): versioned, department-scoped prompt
// templates in the capability hub. They follow the same pipeline as
// skills (submit → review → publish) and the same version semantics
// as Portkey: an update only creates a new immutable version, and a
// separate publish step marks one version as the production version.
// Agents only ever receive the published version — never a draft.
package store

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MaxPromptContentBytes caps a single template version: templates are
// admin-authored text, not documents.
const MaxPromptContentBytes = 32 * 1024

// MaxPromptArguments caps declared placeholders per version.
const MaxPromptArguments = 20

var validPromptName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidPromptName reports whether name is a usable prompt template or
// argument name (lowercase snake_case).
func ValidPromptName(name string) bool { return validPromptName.MatchString(name) }

// PromptArgument is one declared {{placeholder}} of a prompt template
// version. Values are strings only: rendering is plain text
// substitution, and anything else is a caller error (never silently
// coerced).
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// PromptVersion is one immutable revision of a prompt template.
// Versions are append-only: editing a template always creates a new
// version and never rewrites an old one, so a published version can
// always be rolled back to.
type PromptVersion struct {
	Version   int              `json:"version"`
	Content   string           `json:"content"` // template text, {{name}} placeholders
	Arguments []PromptArgument `json:"arguments"`
	CreatedAt time.Time        `json:"created_at"`
	CreatedBy string           `json:"created_by"`
}

// PromptTemplate is a versioned prompt template in the capability
// hub. Status is draft | in_review | published | rejected. Scope
// limits the departments it is served to ("*" = all). MaxDataLevel
// caps the data level the rendered prompt may carry; MinTier is the
// lowest token tier allowed to read it.
type PromptTemplate struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"` // snake_case, unique
	Description      string          `json:"description"`
	Scope            string          `json:"scope"` // comma-separated departments, or "*"
	MaxDataLevel     DataLevel       `json:"max_data_level"`
	MinTier          Tier            `json:"min_tier"`
	Status           string          `json:"status"`
	CurrentVersion   int             `json:"current_version"`
	PublishedVersion int             `json:"published_version"` // 0 = nothing published yet
	Versions         []PromptVersion `json:"versions"`
	AuthorID         string          `json:"author_id"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// Version returns the version record for n, or nil.
func (t *PromptTemplate) Version(n int) *PromptVersion {
	for i := range t.Versions {
		if t.Versions[i].Version == n {
			return &t.Versions[i]
		}
	}
	return nil
}

// Published returns the currently published version, or nil when
// nothing has been published yet.
func (t *PromptTemplate) Published() *PromptVersion {
	if t.PublishedVersion == 0 {
		return nil
	}
	return t.Version(t.PublishedVersion)
}

// InScope reports whether a caller from department dept may see this
// template ("*" matches every department).
func (t *PromptTemplate) InScope(dept string) bool {
	if t.Scope == "" || t.Scope == "*" {
		return true
	}
	for _, d := range strings.Split(t.Scope, ",") {
		if strings.TrimSpace(d) == dept {
			return true
		}
	}
	return false
}

// ValidateTemplateVersion checks a new version's content against its
// declared arguments: every {{placeholder}} in the content must be a
// declared argument, argument names must be snake_case and unique,
// and count and size must stay within limits.
func ValidateTemplateVersion(content string, args []PromptArgument) error {
	if content == "" {
		return fmt.Errorf("content required")
	}
	if len(content) > MaxPromptContentBytes {
		return fmt.Errorf("content exceeds %d bytes", MaxPromptContentBytes)
	}
	if len(args) > MaxPromptArguments {
		return fmt.Errorf("at most %d arguments", MaxPromptArguments)
	}
	declared := map[string]bool{}
	for _, a := range args {
		if !ValidPromptName(a.Name) {
			return fmt.Errorf("argument %q must be lowercase snake_case", a.Name)
		}
		if declared[a.Name] {
			return fmt.Errorf("duplicate argument %q", a.Name)
		}
		declared[a.Name] = true
	}
	// Every placeholder in the content must be declared; any other
	// {{...}} is a template authoring error, caught here instead of
	// shipping "{{name}}" to a model.
	rest := content
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			return nil
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return fmt.Errorf("unclosed placeholder in content")
		}
		name := rest[i+2 : i+j]
		if !ValidPromptName(name) {
			return fmt.Errorf("malformed placeholder {{%s}}", name)
		}
		if !declared[name] {
			return fmt.Errorf("placeholder {{%s}} is not a declared argument", name)
		}
		rest = rest[i+j+2:]
	}
}

// RenderPrompt substitutes {{name}} placeholders in version v with
// args. Strict like F7's schema validation: a missing required
// argument, an unknown argument, or a non-string value is an error
// naming the field (never its value). Optional arguments left out
// render as "".
func RenderPrompt(v *PromptVersion, args map[string]any) (string, error) {
	declared := map[string]PromptArgument{}
	for _, a := range v.Arguments {
		declared[a.Name] = a
	}
	values := map[string]string{}
	for name, raw := range args {
		if _, ok := declared[name]; !ok {
			return "", fmt.Errorf("unknown argument %q", name)
		}
		s, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("argument %q must be a string", name)
		}
		values[name] = s
	}
	for _, a := range v.Arguments {
		if a.Required {
			if _, ok := values[a.Name]; !ok {
				return "", fmt.Errorf("missing required argument %q", a.Name)
			}
		}
	}
	out := v.Content
	for _, a := range v.Arguments {
		out = strings.ReplaceAll(out, "{{"+a.Name+"}}", values[a.Name])
	}
	return out, nil
}
