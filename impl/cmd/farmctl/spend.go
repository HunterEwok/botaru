// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
)

// spendReconcile reads a CSV of the provider's daily sums per workspace and hands in each UTC
// day with P2 spend.reconcile (BUD-10). The operator exports the provider's cost report and
// converts it to euros in this form: a header day,tenant,workspace,eur, then one row per
// workspace and day.
func spendReconcile(c *client, args []string) error {
	fs := flag.NewFlagSet("spend reconcile", flag.ExitOnError)
	provider := fs.String("provider", "", "the model provider of the report, for example anthropic")
	file := fs.String("file", "", "the CSV file")
	_ = fs.Parse(args)
	if *provider == "" || *file == "" {
		return errors.New("spend reconcile needs -provider and -file")
	}
	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer f.Close()
	r := csv.NewReader(f)
	head, err := r.Read()
	if err != nil {
		return err
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, k := range []string{"day", "tenant", "eur"} {
		if _, ok := col[k]; !ok {
			return fmt.Errorf("the CSV header has no column %s", k)
		}
	}
	days := map[string][]map[string]any{}
	for line := 2; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		eur, err := strconv.ParseFloat(strings.TrimSpace(rec[col["eur"]]), 64)
		if err != nil || eur < 0 {
			return fmt.Errorf("line %d: eur is not a sum", line)
		}
		row := map[string]any{"tenant": strings.TrimSpace(rec[col["tenant"]]), "spent_eur_micros": int64(math.Round(eur * 1e6))}
		if i, ok := col["workspace"]; ok && strings.TrimSpace(rec[i]) != "" {
			row["workspace"] = strings.TrimSpace(rec[i])
		}
		day := strings.TrimSpace(rec[col["day"]])
		days[day] = append(days[day], row)
	}
	for _, day := range slices.Sorted(maps.Keys(days)) {
		if err := c.print("spend.reconcile", map[string]any{"day": day, "provider": *provider, "rows": days[day]}); err != nil {
			return fmt.Errorf("day %s: %w", day, err)
		}
	}
	return nil
}
