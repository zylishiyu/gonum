package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"unicode"

	"golang.org/x/tools/go/packages"
)

var (
	typeNames = flag.String("types", "", "comma-separated list of type names")
	output    = flag.String("output", "", "output file name; default <src dir>/enum.go")
)

func Usage() {
	fmt.Fprintf(os.Stderr, "Usage of gonum:\n")
	fmt.Fprintf(os.Stderr, "\tgonum [flags] -types T [directory]\n")
	fmt.Fprintf(os.Stderr, "\tgonum [flags] -types T files... # Must be a single package\n")
	fmt.Fprintf(os.Stderr, "For more information, see:\n")
	fmt.Fprintf(os.Stderr, "\thttps://github.com/steinfletcher/gonum\n")
	fmt.Fprintf(os.Stderr, "Flags:\n")
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("gonum: ")
	flag.Usage = Usage
	flag.Parse()
	if len(*typeNames) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	typs := strings.Split(*typeNames, ",")

	// We accept either one directory or a list of files. Which do we have?
	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	}

	// Parse the package once.
	var dir string
	g := Generator{}
	if len(args) == 1 && isDirectory(args[0]) {
		dir = args[0]
	}

	g.parsePackage(args)

	// Print the header and package clause.
	g.Printf("// Code generated by \"gonum %s\"; DO NOT EDIT.\n", strings.Join(os.Args[1:], " "))
	g.Printf("\n")
	g.Printf("package %s", g.pkg.name)
	g.Printf("\n")
	g.Printf("import \"encoding/json\"\n")
	g.Printf("import \"errors\"\n")
	g.Printf("import \"fmt\"\n")
	g.Printf("\n")

	// Run generate for each type.
	for _, typeName := range typs {
		g.generate(typeName)
	}

	// Format the output.
	src := g.format()

	// Write to file.
	outputName := *output
	if outputName == "" {
		outputName = filepath.Join(dir, "enum.go")
	}
	err := ioutil.WriteFile(outputName, src, 0644)
	if err != nil {
		log.Fatal(err)
	}
}

// generate produces the enum code for the named type.
func (g *Generator) generate(typeName string) {
	var enums []enum
	for _, file := range g.pkg.files {
		file.enums = nil
		file.typeName = typeName
		ast.Inspect(file.file, file.genDecl)
		if len(file.enums) > 0 {
			enums = append(enums, file.enums...)
		}
	}

	if len(enums) == 0 {
		log.Fatalf("no values defined for type %s", typeName)
	}

	for _, enum := range enums {
		var fields []fieldModel
		for _, field := range enum.elements {

			fields = append(fields, fieldModel{
				Key:         field.name,
				Value:       field.value,
				Description: field.description,
			})
		}

		instanceModel := model{
			InstanceVariable: fmt.Sprintf("%sInstance", lowerFirstChar(enum.newName)),
			OriginalType:     enum.originalName,
			NewType:          enum.newName,
			Fields:           fields,
		}

		g.render(instanceTemplate, instanceModel)
	}
}

func (g *Generator) render(tmpl string, model interface{}) {
	t, err := template.New(tmpl).Parse(tmpl)
	if err != nil {
		log.Fatal("instance template parse: ", err)
	}

	err = t.Execute(&g.buf, model)
	if err != nil {
		log.Fatal("Execute: ", err)
		return
	}
}

func (f *File) genDecl(node ast.Node) bool {
	decl, ok := node.(*ast.GenDecl)
	if !ok || decl.Tok != token.TYPE {
		return true
	}

	for _, spec := range decl.Specs {
		vspec := spec.(*ast.TypeSpec)
		if vspec.Name.Name != f.typeName {
			continue
		}

		if structType, ok := vspec.Type.(*ast.StructType); ok {
			var e *enum
			if structType.Fields != nil {
				for _, field := range structType.Fields.List {
					if field.Tag != nil && strings.HasPrefix(field.Tag.Value, "`enum:") {
						if e == nil {
							e = &enum{
								originalName: vspec.Name.Name,
								newName:      strings.Replace(vspec.Name.Name, "Enum", "", -1),
								elements:     []enumElement{},
							}
						}
						if len(field.Names) > 0 {
							name, description := parseEnumStructTag(field.Tag.Value)
							if name == "-" {
								name = field.Names[0].Name
							}
							e.elements = append(e.elements, enumElement{
								value:       field.Names[0].Name,
								name:        name,
								description: description,
							})
						}
					}
				}
			}

			if e != nil {
				f.enums = append(f.enums, *e)
			}
		}
	}
	return false
}

func parseEnumStructTag(content string) (string, string) {
	if value, ok := parseStructTag(content, "`enum"); ok {
		splits := strings.Split(value, ",")
		name := splits[0]
		var description string
		if len(splits) > 1 {
			description = splits[1]
		}
		return name, description
	}
	log.Fatal("enum struct tag did not contain name")
	return "", ""
}

func parseStructTag(tag string, key string) (value string, ok bool) {
	for tag != "" {
		// Skip leading space.q
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}

		// Scan to colon. A space, a quote or a control character is a syntax error.
		// Strictly speaking, control chars include the range [0x7f, 0x9f], not just
		// [0x00, 0x1f], but in practice, we ignore the multi-byte control characters
		// as it is simpler to inspect the tag's bytes than the tag's runes.
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' && tag[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := string(tag[:i])
		tag = tag[i+1:]

		// Scan quoted string to find value.
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		qvalue := string(tag[:i+1])
		tag = tag[i+1:]

		if key == name {
			value, err := strconv.Unquote(qvalue)
			if err != nil {
				break
			}
			return value, true
		}
	}
	return "", false
}

// parsePackage analyzes the single package constructed from the patterns and tags.
// parsePackage exits if there is an error.
func (g *Generator) parsePackage(patterns []string) {
	cfg := &packages.Config{
		Mode:  packages.LoadSyntax,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		log.Fatal(err)
	}
	if len(pkgs) != 1 {
		log.Fatalf("error: %d packages found", len(pkgs))
	}
	g.addPackage(pkgs[0])
}

// Generator holds the state of the analysis. Primarily used to buffer
// the output for format.Source.
type Generator struct {
	buf bytes.Buffer // Accumulated output.
	pkg *Package     // Package we are scanning.

	trimPrefix  string
	lineComment bool
}

func (g *Generator) Printf(format string, args ...interface{}) {
	_, err := fmt.Fprintf(&g.buf, format, args...)
	if err != nil {
		log.Fatal(err)
	}
}

func isDirectory(name string) bool {
	info, err := os.Stat(name)
	if err != nil {
		log.Fatal(err)
	}
	return info.IsDir()
}

type Package struct {
	name  string
	defs  map[*ast.Ident]types.Object
	files []*File
}

type File struct {
	pkg      *Package  // Package to which this file belongs.
	file     *ast.File // Parsed AST.
	typeName string    // Name of the constant type.
	enums    []enum
}

type enum struct {
	originalName string
	newName      string
	elements     []enumElement
}

type enumElement struct {
	value       string
	name        string
	description string
}

// format returns the gofmt-ed contents of the Generator's buffer.
func (g *Generator) format() []byte {
	src, err := format.Source(g.buf.Bytes())
	if err != nil {
		// Should never happen, but can arise when developing this code.
		// The user can compile the output to see the error.
		log.Printf("warning: internal error: invalid Go generated: %s", err)
		log.Printf("warning: compile the package to analyze the error")
		return g.buf.Bytes()
	}
	return src
}

// addPackage adds a type checked Package and its syntax files to the generator.
func (g *Generator) addPackage(pkg *packages.Package) {
	g.pkg = &Package{
		name:  pkg.Name,
		defs:  pkg.TypesInfo.Defs,
		files: make([]*File, len(pkg.Syntax)),
	}

	for i, file := range pkg.Syntax {
		g.pkg.files[i] = &File{
			file: file,
			pkg:  g.pkg,
		}
	}
}

func lowerFirstChar(in string) string {
	v := []byte(in)
	v[0] = byte(unicode.ToLower(rune(v[0])))
	return string(v)
}

type model struct {
	InstanceVariable string
	OriginalType     string
	NewType          string
	Fields           []fieldModel
}

type fieldModel struct {
	Key         string
	Value       string
	Description string
}

const instanceTemplate = `
type {{.InstanceVariable}}JsonDescriptionModel struct {
	Name string ` + "`json:" + `"name"` + "`" + `
	Description string ` + "`json:" + `"description"` + "`" + `
}

var {{.InstanceVariable}} = {{.OriginalType}}{
{{- range .Fields}}
    {{.Value}}: "{{.Key}}",
{{- end}}
}

// {{.NewType}} is the enum that instances should be created from
type {{.NewType}} struct {
	name  string
	value string
	description string
}

// Enum instances
{{- range $e := .Fields}}
var {{.Value}} = {{$.NewType}}{name: "{{.Key}}", value: "{{.Value}}", description: "{{.Description}}"}
{{- end}}

// New{{.NewType}} generates a new {{.NewType}} from the given display value (name)
func New{{.NewType}}(value string) ({{.NewType}}, error) {
	switch value {
{{- range $e := .Fields}}
	case "{{.Key}}":
		return {{.Value}}, nil
{{- end}}
	default:
		return {{.NewType}}{}, errors.New(
			fmt.Sprintf("'%s' is not a valid value for type", value))
	}
}

// Name returns the enum display value
func (g {{.NewType}}) Name() string {
	switch g {
{{- range $e := .Fields}}
	case {{$e.Value}}:
		return {{$e.Value}}.name
{{- end}}
	default:
		panic("Could not map enum")
	}
}

// String returns the enum display value and is an alias of Name to implement the Stringer interface
func (g {{.NewType}}) String() string {
	return g.Name()
}

// Error returns the enum name and implements the Error interface
func (g {{.NewType}}) Error() string {
	return g.Name()
}

// Description returns the enum description if present. If no description is defined an empty string is returned
func (g {{.NewType}}) Description() string {
switch g {
{{- range $e := .Fields}}
	case {{$e.Value}}:
		return "{{$e.Description}}"
{{- end}}
	default:
		panic("Could not map enum description")
	}
}

// {{.NewType}}Names returns the displays values of all enum instances as a slice
func {{.NewType}}Names() []string {
	return []string{
	{{- range $e := .Fields}}
		"{{.Key}}",
	{{- end}}
	}
}

// {{.NewType}}Values returns all enum instances as a slice
func {{.NewType}}Values() []{{.NewType}} {
	return []{{.NewType}}{
	{{- range $e := .Fields}}
		{{.Value}},
	{{- end}}
	}
}

// MarshalJSON provides json serialization support by implementing the Marshaler interface
func (g {{.NewType}}) MarshalJSON() ([]byte, error) {
	if g.Description() != "" {
		m := {{.InstanceVariable}}JsonDescriptionModel {
			Name: g.Name(),
			Description: g.Description(),
		}
		return json.Marshal(m)
	}
	return json.Marshal(g.Name())
}

// UnmarshalJSON provides json deserialization support by implementing the Unmarshaler interface
func (g *{{.NewType}}) UnmarshalJSON(b []byte) error {
	var v string
	err := json.Unmarshal(b, &v)
	if err != nil {
		return err
	}

	instance, createErr := New{{.NewType}}(v)
	if createErr != nil {
		return createErr
	}

	g.name = instance.name
	g.value = instance.value

	return nil
}
`
