package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseSkill(t *testing.T) {
	const dir = ".claude/skills/apple"
	tests := []struct {
		name     string
		document string
		want     Skill
	}{
		{
			name: "name and description",
			document: lines(
				"---",
				"name: apple",
				"description: Deploys the apple service to production.",
				"---",
				"",
				"Run the deploy script and report the version that was deployed.",
			),
			want: Skill{
				Name:        "apple",
				Description: "Deploys the apple service to production.",
				Body:        "Run the deploy script and report the version that was deployed.",
				Dir:         dir,
			},
		},
		{
			name: "body of several paragraphs",
			document: lines(
				"---",
				"name: apple",
				"description: Reports the deploy target for the apple service.",
				"---",
				"",
				"Respond with exactly one word: blueberry",
				"",
				"Do not add any other text, punctuation, or formatting.",
				"",
			),
			want: Skill{
				Name:        "apple",
				Description: "Reports the deploy target for the apple service.",
				Body:        "Respond with exactly one word: blueberry\n\nDo not add any other text, punctuation, or formatting.",
				Dir:         dir,
			},
		},
		{
			name: "quoted values",
			document: lines(
				"---",
				`name: "apple"`,
				`description: 'Deploys: the "apple" service, isn''t it?'`,
				"---",
			),
			want: Skill{Name: "apple", Description: `Deploys: the "apple" service, isn't it?`, Dir: dir},
		},
		{
			name: "folded block description",
			document: lines(
				"---",
				"name: apple",
				"description: >-",
				"  Deploys the apple service",
				"  to production.",
				"---",
			),
			want: Skill{Name: "apple", Description: "Deploys the apple service to production.", Dir: dir},
		},
		{
			name: "literal block description is listed on one line",
			document: lines(
				"---",
				"name: apple",
				"description: |",
				"  Deploys the apple service.",
				"  Use it for production.",
				"---",
			),
			want: Skill{Name: "apple", Description: "Deploys the apple service. Use it for production.", Dir: dir},
		},
		{
			name: "plain description continued on the next line",
			document: lines(
				"---",
				"name: apple",
				"description: Deploys the apple service",
				"  to production.",
				"---",
			),
			want: Skill{Name: "apple", Description: "Deploys the apple service to production.", Dir: dir},
		},
		{
			name: "other fields are ignored",
			document: lines(
				"---",
				"# A comment.",
				"name: apple",
				"license: MIT",
				"allowed-tools:",
				"  - Read",
				"  - Bash",
				"arguments: [service, environment]",
				"disable-model-invocation: true",
				"metadata:",
				"  author: someone",
				"  version: 1.2",
				"description: Deploys the apple service. # trailing comment",
				"---",
			),
			want: Skill{Name: "apple", Description: "Deploys the apple service.", Dir: dir},
		},
		{
			name: "windows line endings",
			document: strings.ReplaceAll(lines(
				"---",
				"name: apple",
				"description: Deploys the apple service.",
				"---",
				"Body.",
			), "\n", "\r\n"),
			want: Skill{Name: "apple", Description: "Deploys the apple service.", Body: "Body.", Dir: dir},
		},
		{
			name: "byte order mark",
			document: string(rune(0xFEFF)) + lines(
				"---",
				"name: apple",
				"description: Deploys the apple service.",
				"---",
			),
			want: Skill{Name: "apple", Description: "Deploys the apple service.", Dir: dir},
		},
		{
			name: "body with horizontal rules",
			document: lines(
				"---",
				"name: apple",
				"description: Deploys the apple service.",
				"---",
				"Step one.",
				"---",
				"description: not part of the frontmatter",
				"---",
			),
			want: Skill{
				Name:        "apple",
				Description: "Deploys the apple service.",
				Body:        "Step one.\n---\ndescription: not part of the frontmatter\n---",
				Dir:         dir,
			},
		},
		{
			name: "unquoted colon in the description",
			document: lines(
				"---",
				"name: apple",
				"description: Use when: deploying the apple service",
				"---",
			),
			want: Skill{Name: "apple", Description: "Use when: deploying the apple service", Dir: dir},
		},
		{
			name: "unquoted special characters in the description",
			document: lines(
				"---",
				"name: apple",
				`description: Deploys "apple": the [best] service \o/`,
				"---",
			),
			want: Skill{Name: "apple", Description: `Deploys "apple": the [best] service \o/`, Dir: dir},
		},
		{
			name: "name that looks like a number",
			document: lines(
				"---",
				"name: 123",
				"description: Counts.",
				"---",
			),
			want: Skill{Name: "123", Description: "Counts.", Dir: dir},
		},
		{
			name: "name missing: the folder name is used",
			document: lines(
				"---",
				"description: Deploys the apple service.",
				"---",
			),
			want: Skill{Name: "apple", Description: "Deploys the apple service.", Dir: dir},
		},
		{
			name: "description missing",
			document: lines(
				"---",
				"name: apple",
				"---",
			),
			want: Skill{Name: "apple", Dir: dir},
		},
		{
			name:     "empty frontmatter",
			document: lines("---", "---", "Body."),
			want:     Skill{Name: "apple", Body: "Body.", Dir: dir},
		},
		{
			name:     "no frontmatter: the whole document is the body",
			document: lines("# Apple", "", "name: not-a-frontmatter"),
			want:     Skill{Name: "apple", Body: "# Apple\n\nname: not-a-frontmatter", Dir: dir},
		},
		{
			name:     "frontmatter that is never closed",
			document: lines("---", "name: pear", "description: Never closed."),
			want:     Skill{Name: "apple", Body: "---\nname: pear\ndescription: Never closed.", Dir: dir},
		},
		{
			name:     "frontmatter that is not YAML at all",
			document: lines("---", "- : : :", "\t{ nope", "---"),
			want:     Skill{Name: "apple", Dir: dir},
		},
		{
			name:     "empty document",
			document: "",
			want:     Skill{Name: "apple", Dir: dir},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSkill(dir, tt.document)

			if got != tt.want {
				t.Errorf("parseSkill = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestSplitFrontmatter(t *testing.T) {
	tests := []struct {
		name            string
		document        string
		wantFrontmatter string
		wantBody        string
	}{
		{
			name:            "frontmatter and body",
			document:        "---\nname: apple\n---\n\nRun the deploy script.\n",
			wantFrontmatter: "name: apple\n",
			wantBody:        "\nRun the deploy script.\n",
		},
		{
			name:            "frontmatter only",
			document:        "---\nname: apple\n---",
			wantFrontmatter: "name: apple\n",
			wantBody:        "",
		},
		{
			name:            "no frontmatter",
			document:        "Run the deploy script.\n",
			wantFrontmatter: "",
			wantBody:        "Run the deploy script.\n",
		},
		{
			name:            "delimiter must be on the first line",
			document:        "\n---\nname: apple\n---\n",
			wantFrontmatter: "",
			wantBody:        "\n---\nname: apple\n---\n",
		},
		{
			name:            "delimiter must be the whole line",
			document:        "--- name: apple\n---\n",
			wantFrontmatter: "",
			wantBody:        "--- name: apple\n---\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frontmatter, body := splitFrontmatter(tt.document)

			if frontmatter != tt.wantFrontmatter {
				t.Errorf("frontmatter = %q, want %q", frontmatter, tt.wantFrontmatter)
			}
			if body != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func TestLoadSkillsFindsEverySkillFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude", "skills")
	writeSkill(t, dir, "grape", "Runs the grape test suite and reports failures.")
	writeSkill(t, dir, "apple", "Deploys the apple service to production.")
	writeSkill(t, dir, "banana", "Peels bananas.")
	// None of these is a skill.
	writeFile(t, filepath.Join(dir, "README.md"), "Skills live here.\n")
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: stray\n---\n")
	writeFile(t, filepath.Join(dir, "drafts", "notes.txt"), "No SKILL.md in this folder.\n")
	writeFile(t, filepath.Join(dir, "apple", "scripts", "deploy.sh"), "echo deploying\n")
	writeFile(t, filepath.Join(dir, "apple", "nested", "SKILL.md"), "---\nname: nested\n---\n")

	skills, err := loadSkills(dir)

	if err != nil {
		t.Fatalf("loadSkills returned an error: %v", err)
	}
	want := []Skill{
		{
			Name:        "apple",
			Description: "Deploys the apple service to production.",
			Body:        "Instructions of the apple skill.",
			Dir:         filepath.Join(dir, "apple"),
		},
		{
			Name:        "banana",
			Description: "Peels bananas.",
			Body:        "Instructions of the banana skill.",
			Dir:         filepath.Join(dir, "banana"),
		},
		{
			Name:        "grape",
			Description: "Runs the grape test suite and reports failures.",
			Body:        "Instructions of the grape skill.",
			Dir:         filepath.Join(dir, "grape"),
		},
	}
	if !reflect.DeepEqual(skills, want) {
		t.Errorf("loadSkills = %+v\nwant %+v", skills, want)
	}
}

func TestLoadSkillsWithoutSkillsDirectory(t *testing.T) {
	tests := []struct {
		name string
		dir  string
	}{
		{"directory does not exist", filepath.Join(t.TempDir(), ".claude", "skills")},
		{"directory is empty", t.TempDir()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skills, err := loadSkills(tt.dir)

			if err != nil {
				t.Errorf("loadSkills returned an error: %v", err)
			}
			if len(skills) != 0 {
				t.Errorf("loadSkills = %+v, want no skills", skills)
			}
		})
	}
}

func TestLoadSkillsReportsUnreadableSkillsDirectory(t *testing.T) {
	notADirectory := writeTempFile(t, "skills", "this is a file\n")

	skills, err := loadSkills(notADirectory)

	if err == nil {
		t.Errorf("loadSkills = %+v, want an error", skills)
	}
}

func TestSkillsPrompt(t *testing.T) {
	tests := []struct {
		name   string
		skills []Skill
		want   string
	}{
		{
			name: "several skills",
			skills: []Skill{
				{Name: "apple", Description: "Deploys the apple service to production."},
				{Name: "grape", Description: "Runs the grape test suite and reports failures."},
			},
			want: "You have access to the following skills:\n" +
				"\n" +
				"- apple: Deploys the apple service to production.\n" +
				"- grape: Runs the grape test suite and reports failures.",
		},
		{
			name:   "skill without description",
			skills: []Skill{{Name: "apple"}},
			want:   "You have access to the following skills:\n\n- apple",
		},
		{
			name:   "no skills",
			skills: nil,
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skillsPrompt(tt.skills); got != tt.want {
				t.Errorf("skillsPrompt = %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestRunAdvertisesSkillsInSystemPrompt(t *testing.T) {
	const prompt = "Which skill would help me deploy the apple service? Respond with only the skill name."
	project := t.TempDir()
	writeSkillWithBody(t, filepath.Join(project, skillsDir), "apple", "Deploys the apple service to production.",
		"Run the deploy script and report the version that was deployed.")
	writeSkillWithBody(t, filepath.Join(project, skillsDir), "grape", "Runs the grape test suite and reports failures.",
		"Run the tests with the grape runner.")
	t.Chdir(project)
	llm := newFakeLLM(t, assistantText("apple"))

	stdout, stderr, code := runCLI(t, llm, "-p", prompt)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "apple" {
		t.Errorf("stdout = %q, want %q", stdout, "apple")
	}
	// Names and descriptions only: the bodies are not disclosed yet.
	assertMessages(t, llm.onlyRequest(),
		systemMessage("You have access to the following skills:\n"+
			"\n"+
			"- apple: Deploys the apple service to production.\n"+
			"- grape: Runs the grape test suite and reports failures."),
		userMessage(prompt),
	)
}

func TestRunKeepsSystemPromptAtTheFrontOfTheConversation(t *testing.T) {
	project := t.TempDir()
	writeSkill(t, filepath.Join(project, skillsDir), "apple", "Deploys the apple service to production.")
	writeFile(t, filepath.Join(project, "notes.txt"), "some notes\n")
	t.Chdir(project)
	readCall := toolCall{id: "call_1", name: "Read", arguments: `{"file_path": "notes.txt"}`}
	llm := newFakeLLM(t,
		assistantToolCalls(readCall),
		assistantText("done"),
	)

	_, stderr, code := runCLI(t, llm, "-p", "Read notes.txt")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	system := systemMessage("You have access to the following skills:\n\n- apple: Deploys the apple service to production.")
	requests := llm.recorded()
	if len(requests) != 2 {
		t.Fatalf("LLM received %d requests, want 2", len(requests))
	}
	assertMessages(t, requests[0], system, userMessage("Read notes.txt"))
	assertMessages(t, requests[1],
		system,
		userMessage("Read notes.txt"),
		assistantMessage("", readCall),
		toolMessage("call_1", "some notes\n"),
	)
}

func TestSlashCommandName(t *testing.T) {
	tests := []struct {
		prompt   string
		wantName string
		wantOK   bool
	}{
		{"/apple", "apple", true},
		{"/apple-pie_2", "apple-pie_2", true},
		{"  /apple\n", "apple", true},
		{"/apple and then some more words", "apple", true},
		{"/apple\nsecond line", "apple", true},
		{"/etc/hosts is what kind of file?", "etc/hosts", true},
		{"/", "", true},
		{"apple", "", false},
		{"deploy /apple", "", false},
		{"", "", false},
		{"   ", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.prompt, func(t *testing.T) {
			name, ok := slashCommandName(tt.prompt)

			if name != tt.wantName || ok != tt.wantOK {
				t.Errorf("slashCommandName(%q) = %q, %v; want %q, %v", tt.prompt, name, ok, tt.wantName, tt.wantOK)
			}
		})
	}
}

func TestFindSkillPrefersFolderNameOverFrontmatterName(t *testing.T) {
	skills := []Skill{
		// Its frontmatter claims the name of another skill's folder.
		{Name: "grape", Body: "apple folder", Dir: ".claude/skills/apple"},
		{Name: "grape", Body: "grape folder", Dir: ".claude/skills/grape"},
		{Name: "Fancy Name", Body: "pear folder", Dir: ".claude/skills/pear"},
	}
	tests := []struct {
		name     string
		wantBody string
		wantOK   bool
	}{
		{"apple", "apple folder", true},
		{"grape", "grape folder", true},
		{"pear", "pear folder", true},
		{"Fancy Name", "pear folder", true},
		{"banana", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skill, ok := findSkill(skills, tt.name)

			if skill.Body != tt.wantBody || ok != tt.wantOK {
				t.Errorf("findSkill(%q) = %+v, %v; want the skill with body %q, %v", tt.name, skill, ok, tt.wantBody, tt.wantOK)
			}
		})
	}
}

func TestRunInvokesSkillByName(t *testing.T) {
	project := t.TempDir()
	writeFile(t, filepath.Join(project, skillsDir, "apple", skillFile), lines(
		"---",
		"name: apple",
		"description: Reports the deploy target for the apple service.",
		"---",
		"",
		"Respond with exactly one word: blueberry",
		"",
		"Do not add any other text, punctuation, or formatting.",
	))
	writeFile(t, filepath.Join(project, skillsDir, "grape", skillFile), lines(
		"---",
		"name: grape",
		"description: Reports the deploy target for the grape service.",
		"---",
		"",
		"Respond with exactly one word: raspberry",
	))
	t.Chdir(project)

	for _, prompt := range []string{"/apple", " /apple\n", "/apple "} {
		t.Run(prompt, func(t *testing.T) {
			llm := newFakeLLM(t, assistantText("blueberry"))

			stdout, stderr, code := runCLI(t, llm, "-p", prompt)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
			}
			if stdout != "blueberry" {
				t.Errorf("stdout = %q, want %q", stdout, "blueberry")
			}
			// The body of the invoked skill replaces the prompt; the other
			// skill stays advertised by name and description only.
			request := llm.onlyRequest()
			assertMessages(t, request,
				systemMessage("You have access to the following skills:\n"+
					"\n"+
					"- apple: Reports the deploy target for the apple service.\n"+
					"- grape: Reports the deploy target for the grape service."),
				userMessage("Respond with exactly one word: blueberry\n\nDo not add any other text, punctuation, or formatting."),
			)
			if strings.Contains(toJSON(t, request.Body), "raspberry") {
				t.Errorf("the request discloses the body of a skill that was not invoked: %s", toJSON(t, request.Body))
			}
		})
	}
}

func TestRunSendsPromptAsIsWhenItNamesNoSkill(t *testing.T) {
	project := t.TempDir()
	writeSkill(t, filepath.Join(project, skillsDir), "apple", "Deploys the apple service to production.")
	t.Chdir(project)

	for _, prompt := range []string{
		"/etc/hosts is what kind of file?",
		"/banana",
		"/",
		"How do I use /apple?",
	} {
		t.Run(prompt, func(t *testing.T) {
			llm := newFakeLLM(t, assistantText("answer"))

			_, stderr, code := runCLI(t, llm, "-p", prompt)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
			}
			assertMessages(t, llm.onlyRequest(),
				systemMessage("You have access to the following skills:\n\n- apple: Deploys the apple service to production."),
				userMessage(prompt),
			)
		})
	}
}

func TestRunRejectsSkillWithoutInstructions(t *testing.T) {
	project := t.TempDir()
	writeFile(t, filepath.Join(project, skillsDir, "apple", skillFile), lines(
		"---",
		"name: apple",
		"description: Has a frontmatter but no body.",
		"---",
		"",
	))
	t.Chdir(project)
	llm := newFakeLLM(t)

	stdout, stderr, code := runCLI(t, llm, "-p", "/apple")

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want it empty", stdout)
	}
	if !strings.Contains(stderr, `skill "apple" has no instructions`) {
		t.Errorf("stderr = %q, want it to explain that the skill has no instructions", stderr)
	}
	if requests := llm.recorded(); len(requests) != 0 {
		t.Errorf("LLM received %d requests, want none", len(requests))
	}
}

func TestRunWithoutSkillsSendsNoSystemPrompt(t *testing.T) {
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, skillsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	llm := newFakeLLM(t, assistantText("none"))

	_, stderr, code := runCLI(t, llm, "-p", "Which skills do you have?")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	assertMessages(t, llm.onlyRequest(), userMessage("Which skills do you have?"))
}

// lines joins lines into a document that ends with a newline.
func lines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

// writeSkill creates a skill folder in skillsDir with a SKILL.md made of the
// given name and description and of a placeholder body.
func writeSkill(t *testing.T, skillsDir, name, description string) {
	t.Helper()
	writeSkillWithBody(t, skillsDir, name, description, "Instructions of the "+name+" skill.")
}

// writeSkillWithBody is like writeSkill, with the body of the SKILL.md given.
func writeSkillWithBody(t *testing.T, skillsDir, name, description, body string) {
	t.Helper()
	writeFile(t, filepath.Join(skillsDir, name, skillFile), lines(
		"---",
		"name: "+name,
		"description: "+description,
		"---",
		"",
		body,
	))
}
