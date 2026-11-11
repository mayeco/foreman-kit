package evaluator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gojev "github.com/shanehull/go-jev"
	"gopkg.in/yaml.v3"
)

// TemplateFile represents a parsed question set (loaded from .foreman-kit/evaluator.yaml or an external YAML file).
type TemplateFile struct {
	State        any                        `yaml:"state,omitempty" json:"state,omitempty"`
	RawQuestions map[string]RawQuestionSpec `yaml:"questions" json:"questions"`
	Questions    gojev.Questions            `yaml:"-" json:"-"`
}

// RawQuestionSpec holds a YAML question definition before converting to gojev.Question.
type RawQuestionSpec struct {
	Type         string `yaml:"type" json:"type"`
	Instructions string `yaml:"instructions" json:"instructions"`
	Criteria     any    `yaml:"criteria,omitempty" json:"criteria,omitempty"`
}

// NormalizeGateName extracts a canonical gate name from a gate identifier or legacy file path
// (e.g. "gates/plan_review.yaml" -> "plan_review", "canary.yaml" -> "canary").
func NormalizeGateName(fileOrGate string) string {
	s := strings.TrimSpace(fileOrGate)
	if s == "" {
		return ""
	}
	base := filepath.Base(filepath.FromSlash(s))
	ext := filepath.Ext(base)
	if ext == ".yaml" || ext == ".yml" {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// LoadGateTemplate resolves the question set for gateName from pol (.foreman-kit/evaluator.yaml),
// an explicit file override if present on disk, or the embedded seed policy.
func LoadGateTemplate(root, gateName string, pol *Policy) (*TemplateFile, []byte, error) {
	if pol == nil {
		var err error
		pol, err = LoadPolicy(root)
		if err != nil {
			return nil, nil, err
		}
	}
	norm := NormalizeGateName(gateName)
	var gp GatePolicy
	var ok bool
	if norm == "canary" && pol.Canary != nil {
		gp = *pol.Canary
		ok = true
	} else if pol.Gates != nil {
		gp, ok = pol.Gates[norm]
	}

	if ok && gp.File != "" {
		var p string
		if filepath.IsAbs(gp.File) {
			p = gp.File
		} else if root != "" {
			p = filepath.Join(root, filepath.FromSlash(gp.File))
		}
		if p != "" && fileExists(p) {
			raw, err := os.ReadFile(p)
			if err != nil {
				return nil, nil, err
			}
			tf, err := ParseTemplateBytes(raw)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", gp.File, err)
			}
			return tf, raw, nil
		}
	}

	if ok && len(gp.Questions) > 0 {
		tf, raw, err := BuildTemplateFromQuestions(gp.State, gp.Questions)
		if err != nil {
			return nil, nil, fmt.Errorf("gate %s: %w", norm, err)
		}
		return tf, raw, nil
	}

	// Fallback to embedded seed policy if gateName was omitted from a custom partial policy
	if seedPol, err := loadEmbeddedSeedPolicy(); err == nil && seedPol != nil {
		var sgp GatePolicy
		var sok bool
		if norm == "canary" && seedPol.Canary != nil {
			sgp = *seedPol.Canary
			sok = true
		} else if seedPol.Gates != nil {
			sgp, sok = seedPol.Gates[norm]
		}
		if sok && len(sgp.Questions) > 0 {
			tf, raw, err := BuildTemplateFromQuestions(sgp.State, sgp.Questions)
			if err != nil {
				return nil, nil, fmt.Errorf("gate %s: %w", norm, err)
			}
			return tf, raw, nil
		}
	}

	return nil, nil, fmt.Errorf("unknown gate or template %q", gateName)
}

// LoadTemplate parses a question file if it exists on disk, or resolves the corresponding
// gate questions from .foreman-kit/evaluator.yaml.
func LoadTemplate(root, file string) (*TemplateFile, []byte, error) {
	return loadTemplateWithPolicy(root, file, nil)
}

// loadTemplateWithPolicy is LoadTemplate with the gate questions taken from pol; a nil pol loads
// .foreman-kit/evaluator.yaml from root.
func loadTemplateWithPolicy(root, file string, pol *Policy) (*TemplateFile, []byte, error) {
	if filepath.IsAbs(file) && fileExists(file) {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, nil, err
		}
		tf, err := ParseTemplateBytes(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", file, err)
		}
		return tf, raw, nil
	}
	if root != "" {
		local := filepath.Join(root, filepath.FromSlash(file))
		if fileExists(local) {
			raw, err := os.ReadFile(local)
			if err != nil {
				return nil, nil, err
			}
			tf, err := ParseTemplateBytes(raw)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", file, err)
			}
			return tf, raw, nil
		}
	}
	return LoadGateTemplate(root, file, pol)
}

// BuildTemplateFromQuestions validates a map of RawQuestionSpec and returns a TemplateFile and its YAML bytes.
func BuildTemplateFromQuestions(state any, rawQuestions map[string]RawQuestionSpec) (*TemplateFile, []byte, error) {
	tf := TemplateFile{
		State:        state,
		RawQuestions: rawQuestions,
	}
	raw, err := yaml.Marshal(tf)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := ParseTemplateBytes(raw)
	if err != nil {
		return nil, nil, err
	}
	return parsed, raw, nil
}

// ParseTemplateBytes parses and strictly validates a question template YAML payload.
func ParseTemplateBytes(raw []byte) (*TemplateFile, error) {
	var tf TemplateFile
	if err := yaml.Unmarshal(raw, &tf); err != nil {
		return nil, err
	}
	if len(tf.RawQuestions) == 0 {
		return nil, fmt.Errorf("the template defines no questions")
	}

	qs := make(gojev.Questions, len(tf.RawQuestions))
	for id, spec := range tf.RawQuestions {
		t := strings.TrimSpace(spec.Type)
		inst := strings.TrimSpace(spec.Instructions)
		if inst == "" {
			return nil, fmt.Errorf("question %q has empty instructions", id)
		}
		switch t {
		case "noul":
			q := gojev.Noul(inst)
			if spec.Criteria != nil {
				critMap, err := toStringMap(spec.Criteria)
				if err != nil {
					return nil, fmt.Errorf("question %q noul criteria: %w", id, err)
				}
				b, err := json.Marshal(critMap)
				if err != nil {
					return nil, err
				}
				q.Criteria = json.RawMessage(b)
			}
			qs[id] = q
		case "choice":
			critMap, err := toStringMap(spec.Criteria)
			if err != nil || len(critMap) < 2 {
				return nil, fmt.Errorf("question %q choice requires at least 2 criteria options", id)
			}
			qs[id] = gojev.Choice(inst, critMap)
		case "score":
			critList, err := toStringSlice(spec.Criteria)
			if err != nil || len(critList) < 2 {
				return nil, fmt.Errorf("question %q score requires at least 2 rubric levels", id)
			}
			qs[id] = gojev.Score(inst, critList)
		default:
			return nil, fmt.Errorf("question %q has unsupported type %q (expected noul, choice, or score)", id, t)
		}
	}
	tf.Questions = qs
	return &tf, nil
}

// ChoiceCriteria returns the criteria map for a choice or noul question.
func (r RawQuestionSpec) ChoiceCriteria() map[string]string {
	m, _ := toStringMap(r.Criteria)
	return m
}

func toStringMap(v any) (map[string]string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		if ms, ok := v.(map[string]string); ok {
			return ms, nil
		}
		// yaml.v3 decodes a mapping into map[any]any as soon as one key is not a string: the unquoted
		// true/false of a noul's criteria, or the numbered options of a choice. Jev names them by text.
		if mi, ok := v.(map[any]any); ok {
			out := make(map[string]string, len(mi))
			for k, val := range mi {
				if k == nil {
					return nil, fmt.Errorf("expected mapping without a null key")
				}
				name := fmt.Sprint(k)
				if _, twice := out[name]; twice {
					return nil, fmt.Errorf("key %q is given twice", name)
				}
				out[name] = fmt.Sprint(val)
			}
			return out, nil
		}
		return nil, fmt.Errorf("expected mapping")
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[fmt.Sprint(k)] = fmt.Sprint(val)
	}
	return out, nil
}

func toStringSlice(v any) ([]string, error) {
	s, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss, nil
		}
		return nil, fmt.Errorf("expected sequence")
	}
	out := make([]string, 0, len(s))
	for _, item := range s {
		out = append(out, fmt.Sprint(item))
	}
	return out, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
