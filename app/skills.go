package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

const (
	// skillsDir is where a project keeps its skills, relative to the working
	// directory. Each skill is a folder in it.
	skillsDir = ".claude/skills"
	// skillFile is the file that defines a skill inside its folder.
	skillFile = "SKILL.md"
)

// Skill is a folder of instructions that can be loaded into the model's
// context on demand. Only its name and description are always in context.
type Skill struct {
	Name        string
	Description string
	// Body holds the skill's instructions: the SKILL.md without its
	// frontmatter. It reaches the model only when the skill is used.
	Body string
	// Dir is the skill's folder.
	Dir string
}

// skillMetadata is the part of a SKILL.md frontmatter that the program uses.
type skillMetadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// loadSkills discovers the skills kept in dir: every folder in it that holds a
// SKILL.md file. A missing dir simply means that there are no skills.
func loadSkills(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var skills []Skill
	for _, entry := range entries {
		skillDir := filepath.Join(dir, entry.Name())
		document, err := os.ReadFile(filepath.Join(skillDir, skillFile))
		if err != nil {
			continue // not a skill folder
		}
		skills = append(skills, parseSkill(skillDir, string(document)))
	}
	return skills, nil
}

// parseSkill builds the skill kept in dir from the contents of its SKILL.md.
// The folder's name stands in for a name missing from the frontmatter: the
// two are required to match anyway.
func parseSkill(dir, document string) Skill {
	frontmatter, body := splitFrontmatter(document)
	metadata := parseSkillMetadata(frontmatter)

	skill := Skill{
		Name: strings.TrimSpace(metadata.Name),
		// A description may span several lines: keep the listing one per skill.
		Description: strings.Join(strings.Fields(metadata.Description), " "),
		Body:        strings.TrimSpace(body),
		Dir:         dir,
	}
	if skill.Name == "" {
		skill.Name = filepath.Base(dir)
	}
	return skill
}

// splitFrontmatter separates a document into its frontmatter, the YAML between
// the "---" lines that open the document, and its body, everything after it.
// A document without frontmatter is all body.
func splitFrontmatter(document string) (frontmatter, body string) {
	document = strings.TrimPrefix(document, "\ufeff") // byte order mark
	lines := strings.SplitAfter(document, "\n")
	if !isFrontmatterDelimiter(lines[0]) {
		return "", document
	}
	for i := 1; i < len(lines); i++ {
		if isFrontmatterDelimiter(lines[i]) {
			return strings.Join(lines[1:i], ""), strings.Join(lines[i+1:], "")
		}
	}
	return "", document // never closed: not a frontmatter after all
}

func isFrontmatterDelimiter(line string) bool {
	return strings.TrimRight(line, " \t\r\n") == "---"
}

// parseSkillMetadata reads the fields of a frontmatter. Frontmatters are
// written by hand and often are not quite valid YAML, typically because of a
// colon in an unquoted description, so a failed parse is retried once with the
// suspicious values quoted. What still cannot be read is left empty.
func parseSkillMetadata(frontmatter string) skillMetadata {
	var metadata skillMetadata
	if err := yaml.Unmarshal([]byte(frontmatter), &metadata); err != nil {
		metadata = skillMetadata{}
		// The error of the retry is not reported: fields that could be read
		// are kept, and the others stay empty.
		_ = yaml.Unmarshal([]byte(quoteSpecialValues(frontmatter)), &metadata)
	}
	return metadata
}

var (
	// keyValueLine matches a top-level "key: value" line of a frontmatter.
	keyValueLine = regexp.MustCompile(`^([A-Za-z0-9_-]+):[ \t]+(.*\S)[ \t]*$`)
	// yamlSpecial matches the characters that give a plain value another
	// meaning in YAML.
	yamlSpecial = regexp.MustCompile("[{}\\[\\]*&#!|>%@`]|: ")
)

// quoteSpecialValues quotes the value of every top-level "key: value" line
// whose value contains characters that are special in YAML, so that it is
// read as plain text.
func quoteSpecialValues(frontmatter string) string {
	lines := strings.Split(frontmatter, "\n")
	for i, line := range lines {
		match := keyValueLine.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if match == nil {
			continue
		}
		key, value := match[1], match[2]
		if isQuoted(value) || !yamlSpecial.MatchString(value) {
			continue
		}
		escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
		lines[i] = key + `: "` + escaped + `"`
	}
	return strings.Join(lines, "\n")
}

func isQuoted(value string) bool {
	if len(value) < 2 {
		return false
	}
	first, last := value[0], value[len(value)-1]
	return first == last && (first == '"' || first == '\'')
}

// findSkill returns the skill that goes by name: the name of its folder or,
// failing that, the name given in its frontmatter.
func findSkill(skills []Skill, name string) (Skill, bool) {
	for _, skill := range skills {
		if filepath.Base(skill.Dir) == name {
			return skill, true
		}
	}
	for _, skill := range skills {
		if skill.Name == name {
			return skill, true
		}
	}
	return Skill{}, false
}

// parseSlashCommand splits a prompt of the form "/name arguments" into the
// name, its first word, and the arguments, everything after it. ok is false
// for a prompt that does not start with a slash.
func parseSlashCommand(prompt string) (name, arguments string, ok bool) {
	command, isCommand := strings.CutPrefix(strings.TrimSpace(prompt), "/")
	if !isCommand {
		return "", "", false
	}
	if end := strings.IndexFunc(command, unicode.IsSpace); end >= 0 {
		return command[:end], strings.TrimSpace(command[end:]), true
	}
	return command, "", true
}

// expandSlashCommand turns a prompt that invokes a skill, "/name arguments",
// into the instructions of that skill with the arguments filled in, which the
// model gets instead of the raw prompt. Any other prompt is returned as is,
// including one that merely starts with a slash, such as a question about a
// file path.
func expandSlashCommand(prompt string, skills []Skill) (string, error) {
	name, arguments, ok := parseSlashCommand(prompt)
	if !ok {
		return prompt, nil
	}
	skill, ok := findSkill(skills, name)
	if !ok {
		return prompt, nil
	}
	if skill.Body == "" {
		return "", fmt.Errorf("skill %q has no instructions", name)
	}
	return substituteArguments(skill.Body, arguments), nil
}

// argumentPlaceholder matches the placeholders of a skill's body: $ARGUMENTS,
// $ARGUMENTS[N] and its shorthand $N.
var argumentPlaceholder = regexp.MustCompile(`\$ARGUMENTS\[(\d+)\]|\$ARGUMENTS|\$(\d+)`)

// substituteArguments fills the placeholders of a skill's body with the
// arguments the skill was invoked with: $ARGUMENTS stands for all of them, as
// typed, while $ARGUMENTS[N] and $N stand for the N-th whitespace-separated
// one, counting from zero, and for nothing if there are fewer arguments.
//
// Arguments given to a body without any placeholder are appended to it, so
// that the model still gets to see them.
func substituteArguments(body, arguments string) string {
	words := strings.Fields(arguments)
	word := func(position string) string {
		i, err := strconv.Atoi(position)
		if err != nil || i >= len(words) {
			return ""
		}
		return words[i]
	}

	substituted := false
	body = argumentPlaceholder.ReplaceAllStringFunc(body, func(placeholder string) string {
		substituted = true
		groups := argumentPlaceholder.FindStringSubmatch(placeholder)
		switch {
		case groups[1] != "":
			return word(groups[1])
		case groups[2] != "":
			return word(groups[2])
		default:
			return arguments
		}
	})
	if !substituted && arguments != "" {
		body += "\n\nARGUMENTS: " + arguments
	}
	return body
}

// skillsPrompt returns the system prompt that advertises the skills to the
// model, or "" if there is no skill. It discloses names and descriptions only:
// the instructions of a skill are loaded when the skill is used.
func skillsPrompt(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	lines := make([]string, len(skills))
	for i, skill := range skills {
		lines[i] = "- " + skill.Name
		if skill.Description != "" {
			lines[i] += ": " + skill.Description
		}
	}
	return "You have access to the following skills:\n\n" + strings.Join(lines, "\n")
}
