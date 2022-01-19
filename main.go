package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"go/types"
	"io"
	"log"
	"os"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

var (
	pointerReceiverF = flag.Bool("pointer-receiver", false, "the generated receiver type")

	typesF  typesVal
	skipsF  skipsVal
	outputF outputVal
)

type typesVal []string

func (f *typesVal) String() string {
	return strings.Join(*f, ",")
}

func (f *typesVal) Set(v string) error {
	*f = append(*f, v)
	return nil
}

type skipsVal []skips

func (f *skipsVal) String() string {
	parts := make([]string, 0, len(*f))
	for _, m := range *f {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		parts = append(parts, strings.Join(keys, ","))
	}

	return strings.Join(parts, ",")
}

func (f *skipsVal) Set(v string) error {
	parts := strings.Split(v, ",")
	set := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		set[p] = struct{}{}
	}

	*f = append(*f, set)

	return nil
}

type skips map[string]struct{}

func (s skips) Contains(sel string) bool {
	if _, ok := s[sel]; ok {
		return ok
	}

	return false
}

type outputVal struct {
	file *os.File
	name string
}

func (f *outputVal) String() string {
	return f.name
}

func (f *outputVal) Set(v string) error {
	if v == "-" || v == "" {
		f.name = "stdout"

		if f.file != nil {
			_ = f.file.Close()
		}
		f.file = nil

		return nil
	}

	file, err := os.OpenFile(v, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return fmt.Errorf("opening file: %v", v)
	}

	f.name = v
	f.file = file

	return nil
}

func (f *outputVal) Open() (io.WriteCloser, error) {
	if f.file == nil {
		f.file = os.Stdout
	} else {
		err := f.file.Truncate(0)
		if err != nil {
			return nil, err
		}
	}

	return f.file, nil
}

func init() {
	flag.Var(&typesF, "type", "the concrete type. Multiple flags can be specified")
	flag.Var(&skipsF, "skip", "comma-separated field/slice/map selectors to shallow copy. Multiple flags can be specified")
	flag.Var(&outputF, "o", "the output file to write to. Defaults to STDOUT")
}

func main() {
	flag.Parse()

	if len(typesF) == 0 || typesF[0] == "" {
		log.Fatalln("no type given")
	}

	if flag.NArg() != 1 {
		log.Fatalln("No package path given")
	}

	b, err := run(flag.Args()[0], typesF, skipsF, *pointerReceiverF)
	if err != nil {
		log.Fatalln("Error generating deep copy method:", err)
	}

	output, err := outputF.Open()
	if err != nil {
		log.Fatalln("Error initializing output file:", err)
	}
	if _, err := output.Write(b); err != nil {
		log.Fatalln("Error writing result to file:", err)
	}
	output.Close()
}

func run(path string, types typesVal, skips skipsVal, pointer bool) ([]byte, error) {
	packages, err := load(path)
	if err != nil {
		return nil, fmt.Errorf("loading package: %v", err)
	}
	if len(packages) == 0 {
		return nil, errors.New("no package found")
	}

	imports := map[string]string{}
	fns := [][]byte{}

	objs := make([]object, len(types))
	for i, kind := range types {
		obj, err := locateType(packages[0].Name, kind, packages[0])
		if err != nil {
			return nil, fmt.Errorf("locating type %q in %q: %v", kind, packages[0].Name, err)
		}
		objs[i] = obj
	}

	for i, obj := range objs {
		var s map[string]struct{}
		if i < len(skips) {
			s = skips[i]
		}

		fn, err := generateFunc(packages[0], obj, imports, s, pointer, objs)
		if err != nil {
			return nil, fmt.Errorf("generating method: %v", err)
		}

		fns = append(fns, fn)
	}

	b, err := generateFile(packages[0], imports, fns)
	if err != nil {
		return nil, fmt.Errorf("generating file content: %v", err)
	}

	return b, nil
}

func load(patterns string) ([]*packages.Package, error) {
	return packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports,
	}, patterns)
}

func generateFunc(p *packages.Package, obj object, imports map[string]string, skips map[string]struct{}, pointer bool, generating []object) ([]byte, error) {
	var buf bytes.Buffer

	var ptr string
	if pointer {
		ptr = "*"
	}
	kind := obj.Obj().Name()

	source := "o"
	fmt.Fprintf(&buf, `// DeepCopy generates a deep copy of %s%s
func (o %s%s) DeepCopy() %s%s {
	var cp %s = %s%s
`, ptr, kind, ptr, kind, ptr, kind, kind, ptr, source)

	walkType(source, "cp", p.Name, obj, &buf, imports, skips, generating, 0, pointer)

	if pointer {
		buf.WriteString("return &cp\n}")
	} else {
		buf.WriteString("return cp\n}")
	}

	return buf.Bytes(), nil
}

func generateFile(p *packages.Package, imports map[string]string, fn [][]byte) ([]byte, error) {
	var file bytes.Buffer

	fmt.Fprintf(&file, "// generated by %s; DO NOT EDIT.\n\npackage %s\n\n", strings.Join(os.Args, " "), p.Name)

	if len(imports) > 0 {
		file.WriteString("import (\n")
		for name, path := range imports {
			if strings.HasSuffix(path, name) {
				fmt.Fprintf(&file, "%q\n", path)
			} else {
				fmt.Fprintf(&file, "%s %q\n", name, path)
			}
		}
		file.WriteString(")\n")
	}

	for _, fn := range fn {
		file.Write(fn)
		file.WriteString("\n\n")
	}

	b, err := format.Source(file.Bytes())
	if err != nil {
		return nil, fmt.Errorf("error formatting source: %w\nsource:\n%s", err, file.String())
	}

	return b, nil
}

type object interface {
	types.Type
	Obj() *types.TypeName
}

type pointer interface {
	Elem() types.Type
}

type methoder interface {
	types.Type
	Method(i int) *types.Func
	NumMethods() int
}

func locateType(x, sel string, p *packages.Package) (object, error) {
	for _, t := range p.TypesInfo.Defs {
		if t == nil {
			continue
		}
		m := exprFilter(t.Type(), sel, x)
		if m == nil {
			continue
		}

		return m, nil
	}

	return nil, errors.New("type not found")
}

func reducePointer(typ types.Type) (types.Type, bool) {
	if pointer, ok := typ.(pointer); ok {
		return pointer.Elem(), true
	}
	return typ, false
}

func objFromType(typ types.Type) object {
	typ, _ = reducePointer(typ)

	m, ok := typ.(object)
	if !ok {
		return nil
	}

	return m
}

func exprFilter(t types.Type, sel string, x string) object {
	m := objFromType(t)
	if m == nil {
		return nil
	}

	obj := m.Obj()
	if obj.Pkg() == nil || x != obj.Pkg().Name() || sel != obj.Name() {
		return nil
	}

	return m
}

func walkType(source, sink, x string, m types.Type, w io.Writer, imports map[string]string, skips skips, generating []object, depth int, isPtrRecv bool) {
	initial := depth == 0
	if m == nil {
		return
	}

	var needExported bool
	switch v := m.(type) {
	case *types.Named:
		if v.Obj().Pkg() != nil && v.Obj().Pkg().Name() != x {
			needExported = true
		}
	}

	if v, ok := m.(methoder); ok && !initial && reuseDeepCopy(source, sink, v, false, generating, w, isPtrRecv) {
		return
	}

	depth++
	under := m.Underlying()
	switch v := under.(type) {
	case *types.Struct:
		for i := 0; i < v.NumFields(); i++ {
			field := v.Field(i)
			if needExported && !field.Exported() {
				continue
			}
			fname := field.Name()
			sel := sink + "." + fname
			sel = sel[strings.Index(sel, ".")+1:]
			if _, ok := skips[sel]; ok {
				continue
			}
			walkType(source+"."+fname, sink+"."+fname, x, field.Type(), w, imports, skips, generating, depth, isPtrRecv)
		}
	case *types.Slice:
		kind := getElemType(v.Elem(), x, imports)

		idx := "i"
		if depth > 1 {
			idx += strconv.Itoa(depth)
		}

		// sel is only used for skips
		sel := "[i]"
		sel = sel[strings.Index(sel, ".")+1:]
		if !initial {
			sel = sink + sel
		}

		var skipSlice bool
		if skips.Contains(sel) {
			skipSlice = true
		}

		fmt.Fprintf(w, `if %s != nil {
	%s = make([]%s, len(%s))
`, source, sink, kind, source)

		fmt.Fprintf(w, `copy(%s, %s)
`, sink, source)

		var b bytes.Buffer

		if !skipSlice {
			baseSel := "[" + idx + "]"
			walkType(source+baseSel, sink+baseSel, x, v.Elem(), &b, imports, skips, generating, depth, isPtrRecv)
		}

		if b.Len() > 0 {
			fmt.Fprintf(w, `    for %s := range %s {
`, idx, source)

			_, err := b.WriteTo(w)
			if err != nil {
				fmt.Println(err)
			}

			fmt.Fprintf(w, "}\n")
		}

		fmt.Fprintf(w, "}\n")
	case *types.Pointer:
		fmt.Fprintf(w, "if %s != nil {\n", source)

		if e, ok := v.Elem().(methoder); !ok || initial || !reuseDeepCopy(source, sink, e, true, generating, w, isPtrRecv) {
			kind := getElemType(v.Elem(), x, imports)

			fmt.Fprintf(w, `%s = new(%s)
	*%s = *%s
`, sink, kind, sink, source)

			walkType(source, sink, x, v.Elem(), w, imports, skips, generating, depth, isPtrRecv)
		}

		fmt.Fprintf(w, "}\n")
	case *types.Chan:
		kind := getElemType(v.Elem(), x, imports)

		fmt.Fprintf(w, `if %s != nil {
	%s = make(chan %s, cap(%s))
}
`, source, sink, kind, source)
	case *types.Map:
		kkind := getElemType(v.Key(), x, imports)
		vkind := getElemType(v.Elem(), x, imports)

		key, val := "k", "v"

		if depth > 1 {
			key += strconv.Itoa(depth)
			val += strconv.Itoa(depth)
		}

		// Sel is only used for skips
		sel := "[k]"
		if !initial {
			sel = sink + sel
		}
		sel = sel[strings.Index(sel, ".")+1:]

		var skipKey, skipValue bool
		if skips.Contains(sel) {
			skipKey, skipValue = true, true
		}

		fmt.Fprintf(w, `if %s != nil {
	%s = make(map[%s]%s, len(%s))
	for %s, %s := range %s {
`, source, sink, kkind, vkind, source, key, val, source)

		ksink, vsink := key, val

		var b bytes.Buffer

		if !skipKey {
			copyKSink := selToIdent(sink) + "_" + key
			walkType(key, copyKSink, x, v.Key(), &b, imports, skips, generating, depth, isPtrRecv)

			if b.Len() > 0 {
				ksink = copyKSink
				fmt.Fprintf(w, "var %s %s\n", ksink, kkind)
				fmt.Fprintf(w, "%s =  %s\n", ksink, source)
				_, err := b.WriteTo(w)
				if err != nil {
					fmt.Println(err)
				}
			}
		}

		b.Reset()

		if !skipValue {
			copyVSink := selToIdent(sink) + "_" + val
			walkType(val, copyVSink, x, v.Elem(), &b, imports, skips, generating, depth, isPtrRecv)

			if b.Len() > 0 {
				vsink = copyVSink
				fmt.Fprintf(w, "var %s %s\n", vsink, vkind)
				fmt.Fprintf(w, "%s =  %s\n", vsink, val)
				_, err := b.WriteTo(w)
				if err != nil {
					fmt.Println(err)
				}
			}
		}

		fmt.Fprintf(w, "%s[%s] = %s", sink, ksink, vsink)

		fmt.Fprintf(w, "}\n}\n")
	}

}

func getElemType(t types.Type, x string, imports map[string]string) string {
	kind := types.TypeString(t, func(p *types.Package) string {
		name := p.Name()
		if name != x {
			if path, ok := imports[name]; ok && path != p.Path() {
				name = strings.ReplaceAll(p.Path(), "/", "_")
			}
			imports[name] = p.Path()
			return name
		}
		return ""
	})

	return kind
}

func hasDeepCopy(v methoder, generating []object, isPtrRecv bool) (hasMethod, isPointer bool) {
	for _, t := range generating {
		if types.Identical(v, t) {
			return true, isPtrRecv
		}
	}

	for i := 0; i < v.NumMethods(); i++ {
		m := v.Method(i)
		if m.Name() != "DeepCopy" {
			continue
		}

		sig, ok := m.Type().(*types.Signature)
		if !ok {
			continue
		}

		if sig.Params().Len() != 0 || sig.Results().Len() != 1 {
			continue
		}

		ret := sig.Results().At(0)
		retType, retPointer := reducePointer(ret.Type())
		sigType, _ := reducePointer(sig.Recv().Type())

		if !types.Identical(retType, sigType) {
			return false, false
		}

		return true, retPointer
	}

	return false, false
}

func reuseDeepCopy(source, sink string, v methoder, pointer bool, generating []object, w io.Writer, isPtrRecv bool) bool {
	hasMethod, isPointer := hasDeepCopy(v, generating, isPtrRecv)

	if hasMethod {
		if pointer == isPointer {
			fmt.Fprintf(w, "%s = %s.DeepCopy()\n", sink, source)
		} else if pointer {
			fmt.Fprintf(w, `retV := %s.DeepCopy()
	%s = &retV
`, source, sink)
		} else {
			fmt.Fprintf(w, `{
	retV := %s.DeepCopy()
	%s = *retV
}
`, source, sink)
		}
	}

	return hasMethod
}

func selToIdent(sel string) string {
	sel = strings.ReplaceAll(sel, "]", "")

	return strings.Map(func(r rune) rune {
		switch r {
		case '[', '.':
			return '_'
		default:
			return r
		}
	}, sel)
}
