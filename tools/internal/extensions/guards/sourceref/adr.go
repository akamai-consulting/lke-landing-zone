package sourceref

// adr.go — the ADR index is the authority, and nothing was holding it to that.
//
// docs/adr/README.md carries a table of every decision, and it does real work: it
// records which numbers are RESERVED but unwritten, and it is where the
// convention for the repo's one duplicate number is stated. Both facts are
// load-bearing and neither was checked, so the table could drift from the
// directory it describes and nothing would say so. It had: the 0001 row read
// "*Reserved* — Not written" while 0001-pat-rotation-locus.md sat beside it.
//
// THE DUPLICATE IS NOT THE DEFECT, WHICH IS WHY THIS DOES NOT REPORT IT. Two
// ADRs share one number — both authored the same day, both taking the next free
// one — and the README has a whole section deciding what to do about it: always
// qualify the citation with a parenthetical naming which is meant. That is a
// recorded decision, so renumbering would contradict it. What was missing is
// enforcement: several citations were bare, meant one of the two, and left a
// reader no way to know which. (Not spelled out here as a literal — this guard
// reads its own source and would flag the illustration.)
//
// So the rule generalises rather than naming 0007: a number carried by MORE THAN
// ONE file must be cited with a parenthetical. If the duplicate is ever resolved
// the rule stops applying on its own, and if a second one appears it starts.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

const adrDir = "docs/adr"
const adrIndexPath = adrDir + "/README.md"

var (
	// adrFileExpr matches an ADR filename: four digits, a hyphen, a slug.
	adrFileExpr = regexp.MustCompile(`^(\d{4})-[a-z0-9-]+\.md$`)
	// adrRowExpr matches an index row, capturing the number and whether the row
	// links to a file. A reserved row carries a bare number and no link.
	adrRowExpr = regexp.MustCompile(`^\|\s*(?:\[(\d{4})\]\(([^)]+)\)|(\d{4}))\s*(?:⚠️\s*)?\|`)
	// adrCiteExpr matches a prose citation, with what follows kept so the
	// qualifier can be judged.
	// A LEADING ZERO is required, and it is what keeps upstream ADRs out: apl-core
	// numbers its own by date (`ADR 2026-06-02-release-branch-per-cycle`), which is
	// four digits and not ours. This repo zero-pads from 0001, so the leading zero
	// is the numbering scheme rather than a coincidence to lean on.
	adrCiteExpr = regexp.MustCompile(`\bADR[ -](0\d{3})\b`)
)

// adrRow is one index entry.
type adrRow struct {
	Number string
	File   string // "" when the row is a reservation
}

// adrState is the directory and the index, read once.
type adrState struct {
	Files map[string][]string // number -> filenames carrying it
	Rows  map[string][]adrRow // number -> index rows
}

// readADRs loads the directory and the index. A repo without docs/adr yields a
// zero state and the caller judges nothing — an instance carries the delivered
// docs and none of the ADRs.
func readADRs(repo capability.Repo) (adrState, error) {
	st := adrState{Files: map[string][]string{}, Rows: map[string][]adrRow{}}
	entries, err := repo.ReadDir(adrDir)
	if err != nil {
		return adrState{}, nil
	}
	for _, e := range entries {
		if m := adrFileExpr.FindStringSubmatch(e.Name()); m != nil {
			st.Files[m[1]] = append(st.Files[m[1]], e.Name())
		}
	}
	data, err := repo.ReadFile(adrIndexPath)
	if err != nil {
		return st, nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		m := adrRowExpr.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		num, file := m[1], m[2]
		if num == "" {
			num = m[3] // a reservation: bare number, no link
		}
		st.Rows[num] = append(st.Rows[num], adrRow{Number: num, File: file})
	}
	return st, nil
}

// checkADRIndex compares the index against the directory in both directions.
//
// THREE WAYS THEY CAN DISAGREE, and the third is the one that actually happened:
// a file with no row (invisible in the index), a row whose link does not resolve
// (a dead pointer in the one place a reader starts), and a row that says
// "reserved, not written" beside a file that exists. The last is the worst,
// because it reads as a deliberate statement of absence and is simply false.
func checkADRIndex(st adrState) []string {
	var out []string
	for num, files := range st.Files {
		rows := st.Rows[num]
		if len(rows) == 0 {
			out = append(out, fmt.Sprintf("%s: %s exists but the index has no row for %s",
				adrIndexPath, files[0], num))
			continue
		}
		linked := map[string]bool{}
		for _, r := range rows {
			if r.File != "" {
				linked[r.File] = true
			}
		}
		for _, f := range files {
			if !linked[f] {
				out = append(out, fmt.Sprintf("%s: %s exists, but the %s row records it as "+
					"reserved/unwritten or links elsewhere — the index states an absence that is not true",
					adrIndexPath, f, num))
			}
		}
	}
	for num, rows := range st.Rows {
		for _, r := range rows {
			if r.File == "" {
				if len(st.Files[num]) > 0 {
					continue // already reported above
				}
				continue // a genuine reservation: no row link, no file
			}
			found := false
			for _, f := range st.Files[num] {
				if f == r.File {
					found = true
				}
			}
			if !found {
				out = append(out, fmt.Sprintf("%s: the %s row links %s, which is not in %s/",
					adrIndexPath, num, r.File, adrDir))
			}
		}
	}
	sort.Strings(out)
	return out
}

// adrCitationFinding judges one `ADR NNNN` citation. Returns "" when it is fine.
//
// A number the index does not carry is a citation nobody can follow. A number
// carried by more than one FILE must be qualified — see this file's header for
// why the duplicate itself is left alone.
func adrCitationFinding(st adrState, file, line string, m []int) string {
	num := line[m[2]:m[3]]
	if len(st.Rows) == 0 {
		return ""
	}
	// TWO FILES TEACH THE RULE AND MUST NOT BE JUDGED BY IT, which is the shape
	// docs-guard's header warns about when it declines to report unknown commands:
	// flagging the document that explains a convention is how a guard gets deleted.
	//
	//   the INDEX states the qualify-your-citation rule, and has to quote a bare
	//   citation to do it. It is judged structurally by checkADRIndex instead.
	//   an ADR's OWN TITLE names its own number. A document does not disambiguate
	//   itself; it is the thing being disambiguated.
	if file == adrIndexPath {
		return ""
	}
	if strings.HasPrefix(file, adrDir+"/"+num+"-") {
		return ""
	}
	if len(st.Rows[num]) == 0 {
		return fmt.Sprintf("ADR %s is not in %s — cite a decision the index carries, "+
			"or add the row if it is a new reservation", num, adrIndexPath)
	}
	if len(st.Files[num]) < 2 {
		return ""
	}
	// Ambiguous: require a parenthetical, or a citation that already names the file.
	rest := strings.TrimLeft(line[m[1]:], " ")
	if strings.HasPrefix(rest, "(") || strings.Contains(line, "-"+num[:0]) {
		return ""
	}
	if strings.Contains(line, num+"-") { // links the file directly
		return ""
	}
	return fmt.Sprintf("ADR %s is carried by %d files, so a bare citation is ambiguous — "+
		"qualify it the way %s prescribes, e.g. `ADR %s (state encryption)`",
		num, len(st.Files[num]), adrIndexPath, num)
}
