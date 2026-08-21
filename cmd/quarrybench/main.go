// Command quarrybench measures the query layer against the storage engine
// underneath it.
//
// The numbers that matter here are not raw key-value throughput — that belongs
// to lsmdb — but what the SQL layer costs on top of it, and whether the
// planner's access-path choices show up as the difference they should be. A
// point lookup and a full scan over the same table answering the same question
// is the clearest version of that comparison, so it is the one this benchmark
// runs.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/dhananjaypesu/quarry"
)

func main() {
	dir := flag.String("db", "benchdata", "database directory")
	n := flag.Int("n", 100000, "rows to load")
	queries := flag.Int("q", 20000, "point queries to run")
	batch := flag.Int("batch", 500, "rows per transaction during load")
	keep := flag.Bool("keep", false, "keep the database directory afterwards")
	flag.Parse()

	if !*keep {
		os.RemoveAll(*dir)
		defer os.RemoveAll(*dir)
	}
	db, err := quarry.Open(*dir, quarry.DefaultOptions())
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	mustExec(db, `
CREATE TABLE bench (id INT PRIMARY KEY, tag TEXT NOT NULL, amount FLOAT, note TEXT);
`)

	tags := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	rng := rand.New(rand.NewSource(1))

	// Loading in batches keeps one transaction per batch, which is where the
	// storage engine's single-fsync-per-batch design pays off.
	start := time.Now()
	for i := 0; i < *n; i += *batch {
		var sb strings.Builder
		sb.WriteString("BEGIN; INSERT INTO bench VALUES ")
		for j := 0; j < *batch && i+j < *n; j++ {
			if j > 0 {
				sb.WriteString(",")
			}
			id := i + j
			fmt.Fprintf(&sb, "(%d,'%s',%.2f,'row %d padding padding')",
				id, tags[rng.Intn(len(tags))], rng.Float64()*1000, id)
		}
		sb.WriteString("; COMMIT;")
		mustExec(db, sb.String())
	}
	report("load", *n, time.Since(start))

	start = time.Now()
	mustExec(db, "CREATE INDEX bench_tag ON bench (tag)")
	report("build index", *n, time.Since(start))

	// Point lookups: the planner should turn each one into a single-key range.
	start = time.Now()
	for i := 0; i < *queries; i++ {
		id := rng.Intn(*n)
		res, err := db.Query(fmt.Sprintf("SELECT amount FROM bench WHERE id = %d", id))
		if err != nil {
			fatal(err)
		}
		if len(res.Rows) != 1 {
			fatal(fmt.Errorf("lookup of id %d returned %d rows", id, len(res.Rows)))
		}
	}
	report("point lookups", *queries, time.Since(start))

	start = time.Now()
	res, err := db.Query("SELECT COUNT(*), AVG(amount) FROM bench")
	if err != nil {
		fatal(err)
	}
	report("full scan aggregate", *n, time.Since(start))
	fmt.Printf("  -> %s rows, average %s\n", res.Rows[0][0], res.Rows[0][1])

	start = time.Now()
	res, err = db.Query("SELECT COUNT(*) FROM bench WHERE tag = 'alpha'")
	if err != nil {
		fatal(err)
	}
	report("index scan on tag", int(res.Rows[0][0].I), time.Since(start))

	start = time.Now()
	res, err = db.Query("SELECT tag, COUNT(*), AVG(amount) FROM bench GROUP BY tag")
	if err != nil {
		fatal(err)
	}
	report("group by tag", *n, time.Since(start))
	fmt.Printf("  -> %d groups\n", len(res.Rows))

	s := db.Stats()
	tables := 0
	for _, t := range s.TablesPerLevel {
		tables += t
	}
	fmt.Printf("\nstorage: %d sstables over %d levels, %d B user data, %d B written, %.2fx write amplification\n",
		tables, len(s.TablesPerLevel), s.UserBytes, s.WALBytes+s.TableBytes, s.WriteAmplification())
}

func report(what string, n int, d time.Duration) {
	if n <= 0 {
		n = 1
	}
	fmt.Printf("%-24s %8s  %10.0f ops/s  %8s/op\n", what, d.Round(time.Millisecond),
		float64(n)/d.Seconds(), (d / time.Duration(n)).Round(time.Nanosecond))
}

func mustExec(db *quarry.DB, script string) {
	if _, err := db.Exec(script); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "quarrybench: %v\n", err)
	os.Exit(1)
}
