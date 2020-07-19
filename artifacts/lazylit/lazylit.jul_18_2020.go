// Commit: 1f1a39ac4217e834caa42b7a50961802dc593f18
// CommitDate: Jul 18 2020
// SourceFile: lazylit.go
// SourceLink: https://github.com/dsabsay/lazylit/blob/1f1a39ac4217e834caa42b7a50961802dc593f18/lazylit.go
// DocAuthor: Daniel Sabsay

// **lazylit** is a code documentation tool that generates static HTML
// that can be published via e.g. GitHub Pages and linked from anywhere
// (code comments, READMEs, Jira tickets, etc.).
//
// The code is based on [gocco](https://github.com/nikhilm/gocco) which is a
// Go port of [Docco](http://jashkenas.github.com/docco/).
// It produces HTML that displays your comments
// alongside your code. Comments are passed through
// [Markdown](http://daringfireball.net/projects/markdown/syntax), and code is
// passed through [Pygments](http://pygments.org/) syntax highlighting.
//
// The [source for lazylit](http://github.com/dsabsay/lazylit) is available on
// GitHub, and released under the MIT license.
//
package main

import (
	"bytes"
	"container/list"
	"flag"
	"fmt"
	"github.com/russross/blackfriday"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"text/template"
	"time"
)

// ## Types
// Due to Go's statically typed nature, what is passed around in object
// literals in Docco, requires various structures

// A `Section` captures a piece of documentation and code
// Every time interleaving code is found between two comments
// a new `Section` is created.
type Section struct {
	docsText []byte
	codeText []byte
	DocsHTML []byte
	CodeHTML []byte
}

// a `TemplateSection` is a section that can be passed
// to Go's templating system, which expects strings.
type TemplateSection struct {
	DocsHTML string
	CodeHTML string
	// The `Index` field is used to create anchors to sections
	Index int
}

// a `Language` describes a programming language
type Language struct {
	name           string         // the `Pygments` name of the language
	symbol         string         // The comment delimiter
	commentMatcher *regexp.Regexp // The regular expression to match the comment delimiter
	dividerText    string         // Used as a placeholder so we can parse back Pygments output and put the sections together
	dividerHTML    *regexp.Regexp // The HTML equivalent
	headerParser   *regexp.Regexp // Extracts header values from comment lines
}

// a `TemplateData` is per-file
type TemplateData struct {
	Title          string             // Title of the HTML output
	Sections       []*TemplateSection // The Sections making up this file
	OtherRevisions []ArtifactSnapshot // List of other revisions for same artifact.
	// Only generate the TOC if there is more than one file (`Multiple == true`).
	// Go's templating system does not allow expressions in the
	// template, so calculate it outside
	Multiple bool
	Snapshot *ArtifactSnapshot
}

// a map of all the languages we know
var languages map[string]*Language

// ## Constants
const VERSION = "0.2.1"
const DESCRIPTION = `usage: lazylit [-version]

    Generate source code documentation as static web pages.

    Commented source files must reside in the artifacts/ directory, such as:

        artifacts/
            crazy_makefile/
                Makefile.jul_18_20
                Makefile.apr_1_20
            acrobatic_javascript/
                foo.jul_2_20.js
                foo.jan_14_20.js

    Invoke with no arguments to generate HTML in the docs/ directory.

Flags:
`

// Wrap the code in these
const highlightStart = "<div class=\"highlight\"><pre>"
const highlightEnd = "</pre></div>"

// ## Command-line flags
var versionFlag *bool = flag.Bool("version", false, "Print version info.")
var helpFlag *bool = flag.Bool("help", false, "Print this help message.")

// ## Main documentation generation functions

// Generate the documentation for a single source file
// by splitting it into sections, highlighting each section
// and putting it together.
// The WaitGroup is used to signal we are done, so that the main
// goroutine waits for all the sub goroutines
func generateDocumentation(a ArtifactSnapshot, otherRevs []ArtifactSnapshot, wg *sync.WaitGroup) {
	code, err := ioutil.ReadFile(a.DocFileName)
	if err != nil {
		log.Panic(err)
	}
	sections := parse(a.DocFileName, code, a.FirstNonHeaderLine)
	highlight(a.DocFileName, sections)
	generateHTML(a, otherRevs, sections)
	wg.Done()
}

// Parse splits code into `Section`s
func parse(source string, code []byte, startLine int) *list.List {
	lines := bytes.Split(code, []byte("\n"))
	sections := new(list.List)
	sections.Init()
	language := getLanguage(source)

	var hasCode bool
	var codeText = new(bytes.Buffer)
	var docsText = new(bytes.Buffer)

	// save a new section
	save := func(docs, code []byte) {
		// deep copy the slices since slices always refer to the same storage
		// by default
		docsCopy, codeCopy := make([]byte, len(docs)), make([]byte, len(code))
		copy(docsCopy, docs)
		copy(codeCopy, code)
		sections.PushBack(&Section{docsCopy, codeCopy, nil, nil})
	}

	for i := startLine; i < len(lines); i++ {
		line := lines[i]
		// if the line is a comment
		if language.commentMatcher.Match(line) {
			// but there was previous code
			if hasCode {
				// we need to save the existing documentation and text
				// as a section and start a new section since code blocks
				// have to be delimited before being sent to Pygments
				save(docsText.Bytes(), codeText.Bytes())
				hasCode = false
				codeText.Reset()
				docsText.Reset()
			}
			docsText.Write(language.commentMatcher.ReplaceAll(line, nil))
			docsText.WriteString("\n")
		} else {
			hasCode = true
			codeText.Write(line)
			codeText.WriteString("\n")
		}
	}
	// save any remaining parts of the source file
	save(docsText.Bytes(), codeText.Bytes())
	return sections
}

// `highlight` pipes the source to Pygments, section by section
// delimited by dividerText, then reads back the highlighted output,
// searches for the delimiters and extracts the HTML version of the code
// and documentation for each `Section`
func highlight(source string, sections *list.List) {
	language := getLanguage(source)
	pygments := exec.Command("pygmentize", "-l", language.name, "-f", "html", "-O", "encoding=utf-8")
	pygmentsInput, _ := pygments.StdinPipe()
	pygmentsOutput, _ := pygments.StdoutPipe()
	// start the process before we start piping data to it
	// otherwise the pipe may block
	pygments.Start()
	for e := sections.Front(); e != nil; e = e.Next() {
		pygmentsInput.Write(e.Value.(*Section).codeText)
		if e.Next() != nil {
			io.WriteString(pygmentsInput, language.dividerText)
		}
	}
	pygmentsInput.Close()

	buf := new(bytes.Buffer)
	io.Copy(buf, pygmentsOutput)

	output := buf.Bytes()
	output = bytes.Replace(output, []byte(highlightStart), nil, -1)
	output = bytes.Replace(output, []byte(highlightEnd), nil, -1)

	for e := sections.Front(); e != nil; e = e.Next() {
		index := language.dividerHTML.FindIndex(output)
		if index == nil {
			index = []int{len(output), len(output)}
		}

		fragment := output[0:index[0]]
		output = output[index[1]:]
		e.Value.(*Section).CodeHTML = bytes.Join([][]byte{[]byte(highlightStart), []byte(highlightEnd)}, fragment)
		e.Value.(*Section).DocsHTML = blackfriday.MarkdownCommon(e.Value.(*Section).docsText)
	}
}

// render the final HTML
func generateHTML(a ArtifactSnapshot, otherRevs []ArtifactSnapshot, sections *list.List) {
	// convert every `Section` into corresponding `TemplateSection`
	sectionsArray := make([]*TemplateSection, sections.Len())
	for e, i := sections.Front(), 0; e != nil; e, i = e.Next(), i+1 {
		var sec = e.Value.(*Section)
		docsBuf := bytes.NewBuffer(sec.DocsHTML)
		codeBuf := bytes.NewBuffer(sec.CodeHTML)
		sectionsArray[i] = &TemplateSection{docsBuf.String(), codeBuf.String(), i + 1}
	}
	// run through the Go template
	html := goccoTemplate(TemplateData{
		filepath.Base(a.SourceFileName),
		sectionsArray,
		otherRevs,
		len(otherRevs) > 1,
		&a,
	})
	// Replace *sources* with the revisions for this file
	// html := goccoTemplate(TemplateData{title, sectionsArray, sources, len(sources) > 1})
	log.Println("gocco: ", a.DocFileName, " -> ", a.Destination())
	ioutil.WriteFile(a.Destination(), html, 0644)
}

func goccoTemplate(data TemplateData) []byte {
	// this hack is required because `ParseFiles` doesn't
	// seem to work properly, always complaining about empty templates
	t, err := template.New("gocco").Funcs(
		// introduce the two functions that the template needs
		template.FuncMap{
			"base":        filepath.Base,
			"destination": ArtifactSnapshot.Destination,
		}).Parse(HTML)
	if err != nil {
		panic(err)
	}
	buf := new(bytes.Buffer)
	err = t.Execute(buf, data)
	if err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// get a `Language` given a path
func getLanguage(source string) *Language {
	return languages[filepath.Ext(source)]
}

// make sure `docs/` exists
func ensureDirectory(name string) {
	os.MkdirAll(name, 0755)
}

func setupLanguages() {
	languages = make(map[string]*Language)
	// you can add more languages here.
	// only the first two fields should change, the rest should
	// be `nil, "", nil`
	languages[".go"] = &Language{"go", "//", nil, "", nil, nil}
	languages[".py"] = &Language{"python", "#", nil, "", nil, nil}
}

func setup() {
	setupLanguages()

	// create the regular expressions based on the language comment symbol
	for _, lang := range languages {
		lang.headerParser, _ = regexp.Compile("^\\s*" + lang.symbol + "\\s*(\\w+):\\s*(.*)$")
		lang.commentMatcher, _ = regexp.Compile("^\\s*" + lang.symbol + "\\s?")
		lang.dividerText = "\n" + lang.symbol + "DIVIDER\n"
		lang.dividerHTML, _ = regexp.Compile("\\n*<span class=\"c1?\">" + lang.symbol + "DIVIDER<\\/span>\\n*")
	}
}

// An ArtifactSnapshot represents a single file under the `artifacts/` directory.
// It's intended to represent a given source code file _at a specific point in time_.
type ArtifactSnapshot struct {
	ArtifactName       string
	Commit             string
	CommitDate         time.Time
	CommitDateString   string
	SourceFileName     string
	SourceLink         string
	DocFileName        string // name of file under artifacts/
	DocAuthor          string // author of documentation
	Dest               string // name of HTML file
	FirstNonHeaderLine int    // line number of first non-header line
}

// This is how we make `ArtifactSnapshot`s sortable by CommitDate.
// See [Sorting by Functions](https://gobyexample.com/sorting-by-functions).
type byCommitDate []ArtifactSnapshot

func (s byCommitDate) Len() int {
	return len(s)
}

func (s byCommitDate) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s byCommitDate) Less(i, j int) bool {
	return s[i].CommitDate.Before(s[j].CommitDate)
}

func (a ArtifactSnapshot) Destination() string {
	baseName := filepath.Base(a.DocFileName)
	ext := filepath.Ext(baseName)
	destBase := baseName[:len(baseName)-len(ext)]
	return filepath.Join("docs", a.ArtifactName, destBase+".html")
}

type IndexTemplateData struct {
	ArtifactName string
	Snapshots    []ArtifactSnapshot
}

// Each "artifact" in lazylit is stored in its own subdirectory of
// `artifacts/`. For example, `artifacts/crazy_makefile/` might store
// all versions of documentation a particular project's Makefile.
// The reason for this abstraction (instead of an identifier composed of
// e.g. the source repository name and filename) is that this allows for
// the source filename and location to change over time. Lazylit can
// keep track of all such versions under a single "artifact".
//
// Each artifact gets an index page which lists all available documented
// versions.
func generateIndexes(artifacts map[string][]ArtifactSnapshot) {
	t, err := template.New("artifact_index").Funcs(template.FuncMap{
		"base": filepath.Base,
	}).Parse(INDEX_HTML)

	if err != nil {
		log.Fatal(err.Error())
	}
	for name, snapshots := range artifacts {
		ensureDirectory("docs/" + name)
		dest := filepath.Join("docs/" + name + "/index.html")
		f, err := os.Create(dest)
		if err != nil {
			log.Fatal(err.Error())
		}
		err = t.Execute(f, IndexTemplateData{name, snapshots})
		if err != nil {
			log.Fatal(err.Error())
		}
	}
}

// Generates a general "about" page for lazylit.
func generateAbout(artifacts map[string][]ArtifactSnapshot) {
	t, err := template.New("about_page").Parse(ABOUT_HTML)

	if err != nil {
		log.Fatal(err.Error())
	}
	artifactNames := make([]string, 0, len(artifacts))
	for name, _ := range artifacts {
		artifactNames = append(artifactNames, name)
	}

	dest := filepath.Join("docs", "index.html")
	f, err := os.Create(dest)
	if err != nil {
		log.Fatal(err.Error())
	}
	err = t.Execute(f, artifactNames)
	if err != nil {
		log.Fatal(err.Error())
	}
}

// Each file under `artifacts/` must have several headers:
//
// * Commit: SHA of git commit this documentation is written for
// * CommitDate: Date of commit in format: `Jan 2 2006`
// * SourceFile: Filename of original source code (as it is named in production codebase)
// * SourceLink: A URL to the original source code file. Should be static (i.e. contain the commit SHA).
// * DocAuthor: Name of person writing this documentation.
//
// See [here](https://github.com/dsabsay/lazylit-example/blob/master/artifacts/lazylit/lazylit.jul_18_2020.go#L1)
// for an example of how to define the headers.
func parseHeaders(name, file string) (*ArtifactSnapshot, error) {
	data, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(data, []byte("\n"))
	language := getLanguage(file)

	a := ArtifactSnapshot{ArtifactName: name, DocFileName: file}
	isMissing := map[string]bool{
		"Commit":     true,
		"CommitDate": true,
		"SourceFile": true,
		"SourceLink": true,
		"DocAuthor":  true,
	}
	for i, line := range lines {
		matches := language.headerParser.FindStringSubmatch(string(line))
		if matches == nil {
			a.FirstNonHeaderLine = i
			break
		}
		switch matches[1] {
		case "Commit":
			a.Commit = matches[2]
			isMissing["Commit"] = false
		case "CommitDate":
			date, err := time.Parse("Jan 2 2006", matches[2])
			if err != nil {
				log.Printf("Error parsing line: %v", string(line))
				return nil, fmt.Errorf("Unable to parse headers for %v: %v", file, err)
			}
			a.CommitDate = date
			a.CommitDateString = matches[2]
			isMissing["CommitDate"] = false
		case "SourceFile":
			a.SourceFileName = matches[2]
			isMissing["SourceFile"] = false
		case "SourceLink":
			a.SourceLink = matches[2]
			isMissing["SourceLink"] = false
		case "DocAuthor":
			a.DocAuthor = matches[2]
			isMissing["DocAuthor"] = false
		}
	}

	// check for missing headers
	missingHeaders := make([]string, 0, 5)
	for h, missing := range isMissing {
		if missing {
			missingHeaders = append(missingHeaders, h)
		}
	}
	if len(missingHeaders) > 0 {
		return nil, fmt.Errorf("%v is missing headers: %v\n", file, missingHeaders)
	}

	return &a, nil
}

// let's Go!
func main() {
	setup()
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), DESCRIPTION)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *versionFlag {
		fmt.Printf("lazylit version %v\n", VERSION)
		os.Exit(0)
	}
	if *helpFlag {
		flag.Usage()
		os.Exit(0)
	}

	adirs, err := ioutil.ReadDir("artifacts")
	if err != nil {
		if os.IsNotExist(err) {
			log.Fatalf("No artifacts/ directory found.")
		}
		log.Fatal(err.Error())
	}

    // Parse the contents of `artifacts/`, generating a list of
    // [`ArtifactSnapshot`s](#section-35).
	pageCount := 0
	artifacts := make(map[string][]ArtifactSnapshot)
	for _, dir := range adirs {
		path := filepath.Join("artifacts", dir.Name())
		files, err := ioutil.ReadDir(path)
		if err != nil {
			log.Fatal(err.Error())
		}
		for _, file := range files {
			fpath := filepath.Join(path, file.Name())
			snap, err := parseHeaders(dir.Name(), fpath)
			if err != nil {
				log.Fatal(err.Error())
			}
			artifacts[dir.Name()] = append(artifacts[dir.Name()], *snap)
			pageCount += 1
		}
		sort.Sort(sort.Reverse(byCommitDate(artifacts[dir.Name()])))
	}

	ensureDirectory("docs")
	// A `.nojekyll` file ensures GitHub Pages won't run anything through Jekyll.
    // See https://github.blog/2009-12-29-bypassing-jekyll-on-github-pages/.
	f, err := os.Create("docs/.nojekyll")
	f.Close()
	if err != nil && os.IsNotExist(err) {
		log.Fatalf("Unable to create .nojekyll: %v", err)
	}
	generateAbout(artifacts)
	generateIndexes(artifacts)
	ioutil.WriteFile("docs/gocco.css", bytes.NewBufferString(Css).Bytes(), 0755)

	wg := new(sync.WaitGroup)
	wg.Add(pageCount)
	for _, a := range artifacts {
		for i, snapshot := range a {
			otherRevs := make([]ArtifactSnapshot, len(a))
			copy(otherRevs, a)
			copy(otherRevs[i:], otherRevs[i+1:])
			otherRevs = otherRevs[:len(otherRevs)-1]
			go generateDocumentation(snapshot, otherRevs, wg)
		}
	}
	wg.Wait()
}
