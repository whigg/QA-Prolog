// This program implements a compiler for Quantum-Annealing Prolog.  It accepts
// a small subset of Prolog and generates weights for a Hamiltonian expression,
// which can be fed to a quantum annealer such as the D-Wave supercomputer.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
)

//go:generate pigeon -o parser.go parser.peg
//go:generate stringer -type=ASTNodeType

var notify *log.Logger // Help notify the user of warnings and errors.

// Empty is used to treat maps as sets.
type Empty struct{}

// CheckError aborts with an error message if an error value is non-nil.
func CheckError(err error) {
	if err != nil {
		notify.Fatal(err)
	}
}

// BaseName returns a file path with the directory and extension removed.
func BaseName(filename string) string {
	return path.Base(strings.TrimSuffix(filename, path.Ext(filename)))
}

// Parameters encapsulates all command-line parameters as well as various
// global values computed from the AST.
type Parameters struct {
	// Command-line parameters
	ProgName   string   // Name of this program
	InFileName string   // Name of the input file
	WorkDir    string   // Directory for holding intermediate files
	IntBits    uint     // Number of bits to use for each program integer
	Verbose    bool     // Whether to output verbose execution information
	Query      string   // Query to apply to the program
	QmasmArgs  []string // Additional qmasm command-line arguments

	// Computed values
	SymToInt      map[string]int        // Map from a symbol to an integer
	IntToSym      []string              // Map from an integer to a symbol
	TopLevel      map[string][]*ASTNode // Top-level clauses, grouped by name and arity
	SymBits       uint                  // Number of bits to use for each symbol
	OutFileBase   string                // Base name (no path or extension) for output files
	DeleteWorkDir bool                  // Whether to delete WorkDir at the end of the program
}

// ParseError reports a parse error at a given position.
var ParseError func(pos position, format string, args ...interface{})

// VerbosePrintf outputs a message only if verbose output is enabled.
func VerbosePrintf(p *Parameters, fmt string, args ...interface{}) {
	if !p.Verbose {
		return
	}
	notify.Printf("INFO: "+fmt, args...)
}

func main() {
	// Parse the command line.
	p := Parameters{}
	p.ProgName = BaseName(os.Args[0])
	notify = log.New(os.Stderr, p.ProgName+": ", 0)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [<options>] [<infile.pl>]\n\n", p.ProgName)
		flag.PrintDefaults()
	}
	flag.StringVar(&p.Query, "query", "", "Prolog query to apply to the program")
	flag.UintVar(&p.IntBits, "int-bits", 0, "minimum integer width in bits")
	flag.StringVar(&p.WorkDir, "work-dir", "", "directory for storing intermediate files (default: "+path.Join(os.TempDir(), "qap-*")+")")
	flag.BoolVar(&p.Verbose, "verbose", false, "output informational messages during execution")
	flag.BoolVar(&p.Verbose, "v", false, "same as -verbose")
	qmasmStr := flag.String("qmasm-args", "", "additional command-line arguments to pass to qmasm")
	flag.Parse()
	if flag.NArg() == 0 {
		p.InFileName = "<stdin>"
	} else {
		p.InFileName = flag.Arg(0)
	}
	p.QmasmArgs = strings.Fields(*qmasmStr)
	ParseError = func(pos position, format string, args ...interface{}) {
		fmt.Fprintf(os.Stderr, "%s:%d:%d: ", p.InFileName, pos.line, pos.col)
		fmt.Fprintf(os.Stderr, format, args...)
		fmt.Fprintln(os.Stderr, "")
		os.Exit(1)
	}

	// Open the input file.
	var r io.Reader = os.Stdin
	if flag.NArg() > 0 {
		f, err := os.Open(p.InFileName)
		CheckError(err)
		defer f.Close()
		r = f
	}

	// If a query was specified, append it to the input file.
	if p.Query != "" {
		q := "?- " + p.Query
		if !strings.HasSuffix(q, ".") {
			q += "."
		}
		r = io.MultiReader(r, strings.NewReader(q))
	}

	// Parse the input file into an AST.
	VerbosePrintf(&p, "Parsing %s as Prolog code", p.InFileName)
	a, err := ParseReader(p.InFileName, r)
	CheckError(err)
	ast := a.(*ASTNode)

	// Preprocess the AST.
	if len(ast.FindByType(QueryType)) == 0 {
		notify.Fatal("A query must be specified")
	}
	ast.RejectUnimplemented(&p)
	ast.StoreAtomNames(&p)
	ast.AdjustIntBits(&p)
	ast.BinClauses(&p)
	VerbosePrintf(&p, "Representing symbols with %d bit(s) and integers with %d bit(s)", p.SymBits, p.IntBits)

	// Perform type inference on the AST.
	nm2tys, clVarTys := ast.PerformTypeInference()

	// Create a working directory and switch to it.
	CreateWorkDir(&p)
	err = os.Chdir(p.WorkDir)
	CheckError(err)

	// Output Verilog code.
	p.OutFileBase = BaseName(p.InFileName)
	vName := p.OutFileBase + ".v"
	vf, err := os.Create(vName)
	CheckError(err)
	VerbosePrintf(&p, "Writing Verilog code to %s", vName)
	ast.WriteVerilog(vf, &p, nm2tys, clVarTys)
	vf.Close()

	// Compile the Verilog code to an EDIF netlist.
	CreateYosysScript(&p)
	VerbosePrintf(&p, "Converting Verilog code to an EDIF netlist")
	RunCommand(&p, "yosys", "-q", "-s", p.OutFileBase+".ys",
		"-b", "edif", "-o", p.OutFileBase+".edif", p.OutFileBase+".v")

	// Compile the EDIF netlist to QMASM code.
	VerbosePrintf(&p, "Converting the EDIF netlist to QMASM code")
	RunCommand(&p, "edif2qmasm", "-o", p.OutFileBase+".qmasm", p.OutFileBase+".edif")

	// Run the QMASM code and report the results.
	ast.RunQMASM(&p, clVarTys)

	// Optionally remove the working directory.
	if p.DeleteWorkDir {
		err = os.RemoveAll(p.WorkDir)
		CheckError(err)
	}
}
