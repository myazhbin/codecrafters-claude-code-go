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
			Bundled:     true,
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

func TestInvokedSkills(t *testing.T) {
	skills := []Skill{
		{Name: "apple", Dir: ".claude/skills/apple"},
		{Name: "apple-pie_2", Dir: ".claude/skills/apple-pie_2"},
		{Name: "grape", Dir: ".claude/skills/grape"},
	}
	tests := []struct {
		prompt        string
		wantSkills    []string
		wantArguments string
	}{
		// One skill.
		{"/apple", []string{"apple"}, ""},
		{"/apple-pie_2", []string{"apple-pie_2"}, ""},
		{"  /apple\n", []string{"apple"}, ""},
		{"/apple 4127", []string{"apple"}, "4127"},
		{"/apple staging eu-west", []string{"apple"}, "staging eu-west"},
		{"/apple   staging   eu-west  ", []string{"apple"}, "staging   eu-west"},
		{"/apple\tstaging", []string{"apple"}, "staging"},
		{"/apple\nsecond line", []string{"apple"}, "second line"},
		// Stacked skills: the first word that names no skill starts the arguments.
		{"/apple /grape 4127", []string{"apple", "grape"}, "4127"},
		{"/grape /apple 4127", []string{"grape", "apple"}, "4127"},
		{"/apple /grape", []string{"apple", "grape"}, ""},
		{" /apple\n/grape\t 4127 ", []string{"apple", "grape"}, "4127"},
		{"/apple /apple-pie_2 /grape a b", []string{"apple", "apple-pie_2", "grape"}, "a b"},
		{"/apple /apple twice", []string{"apple", "apple"}, "twice"},
		{"/apple 4127 /grape", []string{"apple"}, "4127 /grape"},
		{"/apple /pear 4127", []string{"apple"}, "/pear 4127"},
		{"/apple / /grape", []string{"apple"}, "/ /grape"},
		// No skill.
		{"/pear /apple 4127", nil, "/pear /apple 4127"},
		{"/etc/hosts is what kind of file?", nil, "/etc/hosts is what kind of file?"},
		{"/", nil, "/"},
		{"apple", nil, "apple"},
		{"deploy /apple", nil, "deploy /apple"},
		{"", nil, ""},
		{"   ", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.prompt, func(t *testing.T) {
			invoked, arguments := invokedSkills(tt.prompt, skills)

			var names []string
			for _, skill := range invoked {
				names = append(names, skill.Name)
			}
			if !reflect.DeepEqual(names, tt.wantSkills) || arguments != tt.wantArguments {
				t.Errorf("invokedSkills(%q) = %q, %q; want %q, %q",
					tt.prompt, names, arguments, tt.wantSkills, tt.wantArguments)
			}
		})
	}
}

func TestRunStacksSkills(t *testing.T) {
	project := t.TempDir()
	writeSkillWithBody(t, filepath.Join(project, skillsDir), "apple", "Reports the deploy target for the apple service.",
		"Add this exact line to your response: blueberry-$ARGUMENTS")
	writeSkillWithBody(t, filepath.Join(project, skillsDir), "grape", "Summarizes recent incidents for the payments team.",
		"Add this exact line to your response: cherry-$ARGUMENTS")
	writeSkillWithBody(t, filepath.Join(project, skillsDir), "melon", "Rotates credentials for the notification database.",
		"Add this exact line to your response: papaya-$ARGUMENTS")
	t.Chdir(project)
	system := systemMessage("You have access to the following skills:\n" +
		"\n" +
		"- apple: Reports the deploy target for the apple service.\n" +
		"- grape: Summarizes recent incidents for the payments team.\n" +
		"- melon: Rotates credentials for the notification database.")
	tests := []struct {
		prompt       string
		wantMessages []map[string]any
	}{
		{
			prompt: "/apple /grape 4127",
			wantMessages: []map[string]any{
				system,
				userMessage("Add this exact line to your response: blueberry-4127"),
				userMessage("Add this exact line to your response: cherry-4127"),
			},
		},
		{
			prompt: "/grape /apple 4127",
			wantMessages: []map[string]any{
				system,
				userMessage("Add this exact line to your response: cherry-4127"),
				userMessage("Add this exact line to your response: blueberry-4127"),
			},
		},
		{
			prompt: "/apple 4127 /grape",
			wantMessages: []map[string]any{
				system,
				userMessage("Add this exact line to your response: blueberry-4127 /grape"),
			},
		},
		{
			prompt: "/apple /pear 4127",
			wantMessages: []map[string]any{
				system,
				userMessage("Add this exact line to your response: blueberry-/pear 4127"),
			},
		},
		{
			prompt: "/apple /grape",
			wantMessages: []map[string]any{
				system,
				userMessage("Add this exact line to your response: blueberry-"),
				userMessage("Add this exact line to your response: cherry-"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.prompt, func(t *testing.T) {
			llm := newFakeLLM(t, assistantText("blueberry-4127\ncherry-4127"))

			stdout, stderr, code := runCLI(t, llm, "-p", tt.prompt)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
			}
			if stdout != "blueberry-4127\ncherry-4127" {
				t.Errorf("stdout = %q, want the model's answer", stdout)
			}
			request := llm.onlyRequest()
			assertMessages(t, request, tt.wantMessages...)
			if strings.Contains(toJSON(t, request.Body), "papaya") {
				t.Errorf("the request discloses the body of a skill that was not invoked: %s", toJSON(t, request.Body))
			}
		})
	}
}

func TestSubstituteArguments(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		arguments string
		want      string
	}{
		{
			name:      "all arguments",
			body:      "Respond with exactly: $ARGUMENTS",
			arguments: "4127",
			want:      "Respond with exactly: 4127",
		},
		{
			name:      "positional shorthands in the order the body asks for",
			body:      "Respond with exactly: $1 $0",
			arguments: "mango pear",
			want:      "Respond with exactly: pear mango",
		},
		{
			name:      "indexed arguments",
			body:      "Deploy $ARGUMENTS[1] to $ARGUMENTS[0].",
			arguments: "staging api",
			want:      "Deploy api to staging.",
		},
		{
			name:      "every kind of placeholder in one body",
			body:      "Deploy to $0 in region $1. Full request was: $ARGUMENTS",
			arguments: "staging eu-west",
			want:      "Deploy to staging in region eu-west. Full request was: staging eu-west",
		},
		{
			name:      "repeated placeholders",
			body:      "$0, $0 and $ARGUMENTS[0] again; $ARGUMENTS and $ARGUMENTS",
			arguments: "echo",
			want:      "echo, echo and echo again; echo and echo",
		},
		{
			name:      "placeholders next to other text",
			body:      "($0)[$1]$0$1.$ARGUMENTS!",
			arguments: "a b",
			want:      "(a)[b]ab.a b!",
		},
		{
			name:      "no arguments",
			body:      "Deploy to '$0' ($ARGUMENTS) [$ARGUMENTS[1]].",
			arguments: "",
			want:      "Deploy to '' () [].",
		},
		{
			name:      "fewer arguments than placeholders",
			body:      "first=$0 second=$1 third=$ARGUMENTS[2]",
			arguments: "only",
			want:      "first=only second= third=",
		},
		{
			name:      "positions of several digits",
			body:      "$10 $ARGUMENTS[11] $1",
			arguments: "a b c d e f g h i j k l",
			want:      "k l b",
		},
		{
			name:      "position too large to be a number",
			body:      "[$99999999999999999999999]",
			arguments: "a b",
			want:      "[]",
		},
		{
			name:      "arguments are kept as typed",
			body:      "all=<$ARGUMENTS> second=<$1>",
			arguments: "one   two\tthree",
			want:      "all=<one   two\tthree> second=<two>",
		},
		{
			name:      "arguments that look like placeholders are not expanded again",
			body:      "$0 | $ARGUMENTS",
			arguments: "$1 $ARGUMENTS secret",
			want:      "$1 | $1 $ARGUMENTS secret",
		},
		{
			name:      "an index that is not a number is not a position",
			body:      "$ARGUMENTS[x] $ARGUMENTS[-1]",
			arguments: "a b",
			want:      "a b[x] a b[-1]",
		},
		{
			name:      "body without placeholders gets the arguments appended",
			body:      "Deploy the service.",
			arguments: "staging eu-west",
			want:      "Deploy the service.\n\nARGUMENTS: staging eu-west",
		},
		{
			name:      "body without placeholders and no arguments",
			body:      "Deploy the service for $USER, it costs $ 5.",
			arguments: "",
			want:      "Deploy the service for $USER, it costs $ 5.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := substituteArguments(tt.body, tt.arguments); got != tt.want {
				t.Errorf("substituteArguments(%q, %q) = %q\nwant %q", tt.body, tt.arguments, got, tt.want)
			}
		})
	}
}

func TestRunPassesArgumentsToSkill(t *testing.T) {
	project := t.TempDir()
	writeSkillWithBody(t, filepath.Join(project, skillsDir), "apple", "Echoes its arguments.",
		"Respond with exactly this text and nothing else: $ARGUMENTS")
	writeSkillWithBody(t, filepath.Join(project, skillsDir), "grape", "Swaps its two arguments.",
		"Respond with exactly this text and nothing else: $1 $0")
	t.Chdir(project)
	tests := []struct {
		prompt           string
		wantInstructions string
	}{
		{"/apple 4127", "Respond with exactly this text and nothing else: 4127"},
		{"/grape mango pear", "Respond with exactly this text and nothing else: pear mango"},
		{"/apple staging eu-west", "Respond with exactly this text and nothing else: staging eu-west"},
		{"/apple", "Respond with exactly this text and nothing else: "},
		{"/grape", "Respond with exactly this text and nothing else:  "},
		{"/grape mango", "Respond with exactly this text and nothing else:  mango"},
	}
	for _, tt := range tests {
		t.Run(tt.prompt, func(t *testing.T) {
			llm := newFakeLLM(t, assistantText("ok"))

			_, stderr, code := runCLI(t, llm, "-p", tt.prompt)

			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
			}
			assertMessages(t, llm.onlyRequest(),
				systemMessage("You have access to the following skills:\n"+
					"\n"+
					"- apple: Echoes its arguments.\n"+
					"- grape: Swaps its two arguments."),
				userMessage(tt.wantInstructions),
			)
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

func TestSkillPrompt(t *testing.T) {
	const body = "Run `scripts/checksum.sh $0` using the Bash tool.\n\nRespond with only the value it prints."
	const instructions = "Run `scripts/checksum.sh data.txt` using the Bash tool.\n\nRespond with only the value it prints."
	tests := []struct {
		name  string
		skill Skill
		want  string
	}{
		{
			name:  "skill that bundles files: the message says where its folder is",
			skill: Skill{Name: "apple", Body: body, Dir: ".claude/skills/apple", Bundled: true},
			want: "Skill: apple (located at .claude/skills/apple)\n" +
				"Paths in the instructions below are relative to that folder.\n" +
				"\n" +
				instructions,
		},
		{
			name:  "skill made of instructions alone: the message is the instructions",
			skill: Skill{Name: "apple", Body: body, Dir: ".claude/skills/apple"},
			want:  instructions,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.skill.Prompt("data.txt"); got != tt.want {
				t.Errorf("Prompt = %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestRunExecutesScriptBundledWithSkill(t *testing.T) {
	project := t.TempDir()
	skill := filepath.Join(project, skillsDir, "apple")
	writeFile(t, filepath.Join(skill, skillFile), lines(
		"---",
		"name: apple",
		"description: Reports the size of the project data file.",
		"---",
		"",
		"Run `scripts/size.sh` using the Bash tool.",
		"",
		"Respond with only the value it prints, and nothing else.",
	))
	writeFile(t, filepath.Join(skill, "data.txt"), "some random contents\n")
	script := filepath.Join(skill, "scripts", "size.sh")
	writeFile(t, script, lines(
		"#!/bin/sh",
		`cd "$(dirname "$0")/.." && wc -c < data.txt | tr -d ' '`,
	))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	// The model joins the skill's folder with the path from the instructions.
	runScript := toolCall{id: "call_1", name: "Bash", arguments: `{"command": ".claude/skills/apple/scripts/size.sh"}`}
	llm := newFakeLLM(t,
		assistantToolCalls(runScript),
		assistantText("21"),
	)

	stdout, stderr, code := runCLI(t, llm, "-p", "/apple")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout != "21" {
		t.Errorf("stdout = %q, want %q", stdout, "21")
	}
	system := systemMessage("You have access to the following skills:\n\n- apple: Reports the size of the project data file.")
	instructions := skillMessage("apple", "Run `scripts/size.sh` using the Bash tool.\n\nRespond with only the value it prints, and nothing else.")
	requests := llm.recorded()
	if len(requests) != 2 {
		t.Fatalf("LLM received %d requests, want 2", len(requests))
	}
	assertMessages(t, requests[0], system, instructions)
	assertMessages(t, requests[1],
		system,
		instructions,
		assistantMessage("", runScript),
		toolMessage("call_1", "21\n"),
	)
}

// skillMessage is the user message that hands the instructions of an invoked
// skill of the project to the model, when that skill bundles files.
func skillMessage(name, instructions string) map[string]any {
	return userMessage("Skill: " + name + " (located at .claude/skills/" + name + ")\n" +
		"Paths in the instructions below are relative to that folder.\n" +
		"\n" +
		instructions)
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
