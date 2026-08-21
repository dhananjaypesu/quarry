// Command quarry is an interactive SQL shell.
//
// It reads a statement at a time, sends it to the engine, and prints either a
// table of rows or a count. A statement is complete when it ends in a
// semicolon, so multi-line statements can be pasted as-is.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dhananjaypesu/quarry"
)

func main() {
	dir := flag.String("db", "quarrydata", "database directory")
	command := flag.String("c", "", "run one statement (or a semicolon-separated script) and exit")
	quiet := flag.Bool("q", false, "suppress timings and the banner")
	flag.Parse()

	db, err := quarry.Open(*dir, quarry.DefaultOptions())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	if *command != "" {
		if err := run(db, *command, !*quiet); err != nil {
			fatal(err)
		}
		return
	}
	if fi, _ := os.Stdin.Stat(); fi != nil && fi.Mode()&os.ModeCharDevice == 0 {
		// Piped input is a script: read it whole so that a statement spanning
		// several lines works the same as it does in a file.
		script, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal(err)
		}
		if err := run(db, string(script), !*quiet); err != nil {
			fatal(err)
		}
		return
	}
	repl(db, *quiet)
}

func repl(db *quarry.DB, quiet bool) {
	if !quiet {
		fmt.Println("quarry — SQL on an LSM tree. \\h for help, \\q to quit.")
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	var buf strings.Builder
	for {
		if buf.Len() == 0 {
			fmt.Print("quarry> ")
		} else {
			fmt.Print("     -> ")
		}
		if !in.Scan() {
			fmt.Println()
			return
		}
		line := in.Text()
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 && strings.HasPrefix(trimmed, "\\") {
			if meta(db, trimmed) {
				return
			}
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
		if !strings.HasSuffix(trimmed, ";") {
			continue
		}
		stmt := buf.String()
		buf.Reset()
		if strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(stmt), ";")) == "" {
			continue
		}
		if err := run(db, stmt, !quiet); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
	}
}

// meta handles the backslash commands, returning true when the shell should
// exit.
func meta(db *quarry.DB, line string) bool {
	switch strings.Fields(line)[0] {
	case "\\q", "\\quit", "\\exit":
		return true
	case "\\d", "\\dt":
		printResults(db, "SHOW TABLES;", false)
	case "\\s", "\\stats":
		s := db.Stats()
		tables := 0
		for _, n := range s.TablesPerLevel {
			tables += n
		}
		fmt.Printf("memtable %d B  frozen %d  sstables %d over %d levels  user %d B  on disk %d B  write amplification %.2fx\n",
			s.MemtableBytes, s.FrozenMemtables, tables, len(s.TablesPerLevel),
			s.UserBytes, s.WALBytes+s.TableBytes, s.WriteAmplification())
	case "\\h", "\\help":
		fmt.Println(help)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q, try \\h\n", line)
	}
	return false
}

const help = `statements   SELECT, INSERT, UPDATE, DELETE, CREATE TABLE, CREATE INDEX,
             DROP TABLE, BEGIN, COMMIT, ROLLBACK, EXPLAIN, SHOW TABLES, DESCRIBE
commands     \d list tables   \s storage stats   \h help   \q quit`

func run(db *quarry.DB, script string, timing bool) error {
	return printResults(db, script, timing)
}

func printResults(db *quarry.DB, script string, timing bool) error {
	start := time.Now()
	results, err := db.Exec(script)
	elapsed := time.Since(start)
	for _, res := range results {
		printResult(res)
	}
	if err != nil {
		return err
	}
	if timing && len(results) > 0 {
		fmt.Printf("(%s)\n", elapsed.Round(time.Microsecond))
	}
	return nil
}

func printResult(res quarry.Result) {
	switch res.Kind {
	case quarry.KindText:
		fmt.Println(res.Text)
	case quarry.KindAffected:
		fmt.Printf("%d row(s) affected\n", res.Affected)
	case quarry.KindRows:
		printTable(res)
	}
}

// printTable measures every cell before printing so the columns line up. That
// requires holding the whole result in memory, which the engine already does.
func printTable(res quarry.Result) {
	widths := make([]int, len(res.Columns))
	for i, c := range res.Columns {
		widths[i] = len(c)
	}
	cells := make([][]string, len(res.Rows))
	for i, row := range res.Rows {
		cells[i] = make([]string, len(row))
		for j, v := range row {
			cells[i][j] = v.String()
			if len(cells[i][j]) > widths[j] {
				widths[j] = len(cells[i][j])
			}
		}
	}
	var sb strings.Builder
	for i, c := range res.Columns {
		sb.WriteString(pad(c, widths[i]))
		if i < len(res.Columns)-1 {
			sb.WriteString("  ")
		}
	}
	fmt.Println(strings.TrimRight(sb.String(), " "))
	sb.Reset()
	for i := range res.Columns {
		sb.WriteString(strings.Repeat("-", widths[i]))
		if i < len(res.Columns)-1 {
			sb.WriteString("  ")
		}
	}
	fmt.Println(sb.String())
	for _, row := range cells {
		sb.Reset()
		for j, c := range row {
			sb.WriteString(pad(c, widths[j]))
			if j < len(row)-1 {
				sb.WriteString("  ")
			}
		}
		fmt.Println(strings.TrimRight(sb.String(), " "))
	}
	fmt.Printf("(%d row%s)\n", len(res.Rows), plural(len(res.Rows)))
}

func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "quarry: %v\n", err)
	os.Exit(1)
}
