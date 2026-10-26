package servers

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestInventoryImportStrictFormatsAndNoCredentialDisclosure(t *testing.T) {
	for _, tc := range []struct{ name, format, input string }{
		{"password", "json", `[{"name":"a","host":"a.example","user":"root","pass":"TOP_SECRET"}]`},
		{"key", "json", `[{"key":"TOP_SECRET"}]`},
		{"passphrase", "csv", "name,host,port,user,tags,passphrase\na,a.example,22,root,[],TOP_SECRET\n"},
		{"unknown header", "csv", "name,host,port,user,TOP_SECRET\n"},
		{"duplicate header", "csv", "name,host,port,user,name\n"},
		{"duplicate JSON key", "json", `[{"name":"a","name":"TOP_SECRET","host":"a.example","user":"root"}]`},
		{"case sensitive fields", "json", `[{"Name":"TOP_SECRET"}]`},
		{"object instead of array", "json", `{"name":"TOP_SECRET"}`},
		{"two documents", "json", `[] []`},
		{"truncated JSON", "json", `[{"name":"TOP_SECRET"}`},
		{"null row", "json", `[null]`},
		{"array row", "json", `[[]]`},
		{"non UTF8", "csv", "\xffTOP_SECRET"},
		{"NUL", "json", "[\x00]"},
		{"malformed CSV", "csv", "name,host,port,user,tags\nTOP_SECRET,a,22,root,[],extra\n"},
		{"unterminated CSV", "csv", "name,host,port,user,tags\n\"TOP_SECRET"},
		{"empty", "json", `[]`},
		{"unsupported format", "yaml", "TOP_SECRET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := ParseInventoryImport(strings.NewReader(tc.input), tc.format)
			if err == nil || rows != nil || strings.Contains(err.Error(), "TOP_SECRET") {
				t.Fatalf("parse = %v, %v; want whole-file rejection without input disclosure", rows, err)
			}
		})
	}
}

func TestInventoryImportValidFormatsNormalizeAndReportEveryRow(t *testing.T) {
	for _, format := range []string{"csv", "json"} {
		t.Run(format, func(t *testing.T) {
			input := `[{"name":" Alpha ","host":" [2001:0DB8::1] ","user":" root ","tags":[" prod ","prod","web"]},{"name":"alpha","host":"other.example","user":"root"},{"name":"other","host":"2001:db8:0:0::1","port":22,"user":"root"},{"name":"existing","host":"new.example","user":"root"},{"name":"mapped","host":"::ffff:192.0.2.1","user":"root"},{"name":"bad","host":"https://node.example","port":65536,"user":"root;id"}]`
			if format == "csv" {
				input = "\ufeffname,host,port,user,tags\n Alpha , [2001:0DB8::1] ,, root ,\"[\"\" prod \"\",\"\"prod\"\",\"\"web\"\"]\"\nalpha,other.example,,root,[]\nother,2001:db8:0:0::1,22,root,[]\nexisting,new.example,,root,[]\nmapped,::ffff:192.0.2.1,,root,[]\nbad,https://node.example,65536,root;id,[]\n"
			}
			rows, err := ParseInventoryImport(strings.NewReader(input), format)
			if err != nil {
				t.Fatal(err)
			}
			repo := &fakeRepo{}
			svc, _, _, _ := newTestService(repo, []Server{{Name: "Existing", Host: "192.0.2.1", User: "root", Port: 22}})
			preview, err := NewCommandService(svc).PreviewImport(rows)
			if err != nil || len(preview.Rows) != 6 || len(preview.BatchID) != 32 {
				t.Fatalf("preview = %+v, %v", preview, err)
			}
			first := preview.Rows[0]
			if first.Name != "Alpha" || first.Host != "2001:db8::1" || first.Port != 22 || first.User != "root" || !reflect.DeepEqual(first.Tags, []string{"prod", "web"}) || len(first.Errors) != 0 {
				t.Fatalf("normalized row = %+v", first)
			}
			for index, message := range []string{"Duplicate name", "Duplicate endpoint", "Name already exists", "Endpoint already exists", "Host must"} {
				if !strings.Contains(strings.Join(preview.Rows[index+1].Errors, " "), message) {
					t.Fatalf("row %d errors = %v, want %s", index+1, preview.Rows[index+1].Errors, message)
				}
			}
			if repo.saveCalls != 0 {
				t.Fatal("preview wrote inventory")
			}
		})
	}
}

func TestInventoryImportRowTypesAndLimits(t *testing.T) {
	rows, err := ParseInventoryImport(strings.NewReader(`[{"name":"commas","host":"commas.example","user":"root","tags":["prod,web","web"]}]`), "json")
	if err != nil {
		t.Fatal(err)
	}
	rows = validateImportRows(rows, nil)
	if !reflect.DeepEqual(rows[0].Tags, []string{"prod", "web", "web"}) {
		t.Fatalf("documented legacy tag normalization changed: %v", rows[0].Tags)
	}
	// Boundaries apply to raw bytes, including whitespace, before parsing.
	minimal := `[{"name":"a","host":"a.example","user":"root"}]`
	if _, err := ParseInventoryImport(strings.NewReader(minimal+strings.Repeat(" ", MaxImportBytes-len(minimal))), "json"); err != nil {
		t.Fatalf("exact 2 MiB limit rejected: %v", err)
	}
	for _, input := range []string{
		`[{"name":true,"host":[],"user":{},"port":"22","tags":"prod"}]`,
		`[{"name":null,"host":null,"user":null,"port":null,"tags":null}]`,
		`[{"name":"","host":"","user":"","port":-1}]`,
	} {
		rows, err := ParseInventoryImport(strings.NewReader(input), "json")
		if err != nil {
			t.Fatal(err)
		}
		rows = validateImportRows(rows, nil)
		if len(rows[0].Errors) < 3 {
			t.Fatalf("invalid fields not reported: %+v", rows)
		}
	}
	rows, err = ParseInventoryImport(strings.NewReader("name,host,port,user,tags\na,a.example,no,root,not-an-array\n"), "csv")
	if err != nil || len(rows[0].Errors) < 2 {
		t.Fatalf("CSV field errors = %+v, %v", rows, err)
	}
	if _, err := ParseInventoryImport(strings.NewReader(strings.Repeat(" ", MaxImportBytes+1)), "json"); err == nil {
		t.Fatal("oversized file accepted")
	}
	for _, format := range []string{"json", "csv"} {
		var input strings.Builder
		if format == "csv" {
			input.WriteString("name,host,port,user,tags\n")
		} else {
			input.WriteString("[")
		}
		for i := range MaxImportRows {
			if format == "csv" {
				fmt.Fprintf(&input, "host%d,host%d.example,22,root,[]\n", i, i)
			} else {
				if i != 0 {
					input.WriteString(",")
				}
				fmt.Fprintf(&input, `{"name":"host%d","host":"host%d.example","user":"root"}`, i, i)
			}
		}
		valid := input.String()
		if format == "json" {
			valid += "]"
		}
		rows, err := ParseInventoryImport(strings.NewReader(valid), format)
		if err != nil || len(rows) != MaxImportRows {
			t.Fatalf("1000 rows %s = %d, %v", format, len(rows), err)
		}
		if format == "json" {
			input.WriteString(",{}]")
		} else {
			input.WriteString("extra,extra.example,22,root,[]\n")
		}
		if _, err := ParseInventoryImport(strings.NewReader(input.String()), format); err == nil {
			t.Fatalf("1001 rows %s accepted", format)
		}
	}
}

func TestInventoryImportTagsRequireStringElements(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		for _, tags := range []string{`[null]`, `["prod",null]`, `[null,"prod"]`, `["null","prod"]`} {
			t.Run(format+"/"+tags, func(t *testing.T) {
				input := fmt.Sprintf(`[{"name":"alpha","host":"a.example","user":"root","tags":%s}]`, tags)
				if format == "csv" {
					input = fmt.Sprintf("name,host,port,user,tags\nalpha,a.example,22,root,\"%s\"\n", strings.ReplaceAll(tags, `"`, `""`))
				}
				rows, err := ParseInventoryImport(strings.NewReader(input), format)
				if err != nil {
					t.Fatal(err)
				}
				rows = validateImportRows(rows, nil)
				wantError := tags != `["null","prod"]`
				if (len(rows[0].Errors) != 0) != wantError {
					t.Fatalf("tags %s validation = %+v; want row error=%v", tags, rows[0], wantError)
				}
			})
		}
	}
}

func TestInventoryImportCommitRevalidatesAndPublishesOnlyAfterSave(t *testing.T) {
	repo := &fakeRepo{}
	svc, state, current, statuses := newTestService(repo, nil)
	commands := NewCommandService(svc)
	entries := []ImportRow{{ImportEntry: ImportEntry{Row: 1, Name: "alpha", Host: "a.example", User: "root"}}, {ImportEntry: ImportEntry{Row: 2, Name: "beta", Host: "b.example", User: "root"}}}
	preview, _ := commands.PreviewImport(entries)
	selection := ImportSelection{BatchID: preview.BatchID, Rows: []ImportEntry{preview.Rows[0].ImportEntry, preview.Rows[1].ImportEntry}}
	// A different request claims an equivalent endpoint after the preview.
	if _, err := svc.Create(Server{Name: "concurrent", Host: "A.EXAMPLE", Port: 0, User: "root"}); err != nil {
		t.Fatal(err)
	}
	result := commands.CommitImport(selection)
	if result.Outcome != CommandOutcomeConflict || len(*current) != 1 || len(result.Created) != 0 || len(result.Preview.Rows[0].Errors) == 0 || len(result.Preview.Rows[1].Errors) != 0 {
		t.Fatalf("conflict = %+v, state=%+v", result, *current)
	}
	repo.saveErr = errors.New("database failure with confidential details")
	selection.Rows = selection.Rows[1:]
	result = commands.CommitImport(selection)
	if result.Outcome != CommandOutcomeFailed || len(*current) != 1 || len(*statuses) != 1 || strings.Contains(result.Error, "confidential") {
		t.Fatalf("failure = %+v, state=%+v", result, *current)
	}
	repo.saveErr = nil
	svc.deps.Repository = inspectImportRepository{fakeRepo: repo, inspect: func() {
		if len(state.Servers()) != 1 || len(state.StatusMap()) != 1 {
			t.Fatal("runtime published before transaction success")
		}
	}}
	result = commands.CommitImport(selection)
	if result.Outcome != CommandOutcomeSuccess || len(*current) != 2 || !(*current)[1].Disabled || (*current)[1].Pass != "" || (*current)[1].Key != "" || !(*statuses)["beta"].Disabled {
		t.Fatalf("success = %+v, state=%+v", result, *current)
	}
	if result.Audit.Meta["created_count"] != 1 || result.Audit.Meta["batch_id"] != preview.BatchID || !reflect.DeepEqual(result.Created, []string{"beta"}) {
		t.Fatalf("audit = %+v", result.Audit)
	}
}

type inspectImportRepository struct {
	*fakeRepo
	inspect func()
}

func (r inspectImportRepository) Save(servers []Server, hook TxHook) error {
	r.inspect()
	return r.fakeRepo.Save(servers, hook)
}

func TestInventoryImportConcurrentCommitsCannotCreatePartialBatch(t *testing.T) {
	svc, state, current, _ := newTestService(&fakeRepo{}, nil)
	commands := NewCommandService(svc)
	selections := []ImportSelection{
		{BatchID: strings.Repeat("a", 32), Rows: []ImportEntry{{Row: 1, Name: "first", Host: "same.example", User: "root"}, {Row: 2, Name: "only-first", Host: "first.example", User: "root"}}},
		{BatchID: strings.Repeat("b", 32), Rows: []ImportEntry{{Row: 1, Name: "second", Host: "SAME.EXAMPLE", User: "root"}, {Row: 2, Name: "only-second", Host: "second.example", User: "root"}}},
	}
	results := make(chan ImportResult, 2)
	var workers sync.WaitGroup
	for _, selection := range selections {
		workers.Go(func() { results <- commands.CommitImport(selection) })
	}
	workers.Wait()
	close(results)
	success, conflict := 0, 0
	for result := range results {
		if result.Outcome == CommandOutcomeSuccess {
			success++
		} else if result.Outcome == CommandOutcomeConflict {
			conflict++
		}
	}
	state.Lock()
	defer state.Unlock()
	if success != 1 || conflict != 1 || len(*current) != 2 {
		t.Fatalf("success=%d conflict=%d state=%+v", success, conflict, *current)
	}
}

func TestInventoryImportSelectionRejectsBypassFieldsAndDuplicateRows(t *testing.T) {
	good := `{"batch_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","rows":[{"row":1,"name":"alpha","host":"a.example","user":"root","port":22,"tags":[]}]}`
	selection, err := DecodeImportSelection(strings.NewReader(good))
	if err != nil || len(selection.Rows) != 1 {
		t.Fatalf("valid selection = %+v, %v", selection, err)
	}
	for _, input := range []string{
		strings.Replace(good, `"port":22`, `"disabled":false,"port":22`, 1),
		strings.Replace(good, `"port":22`, `"key":"TOP_SECRET","port":22`, 1),
		strings.Replace(good, `"tags":[]`, `"tags":["prod",null]`, 1),
		strings.Replace(good, `"row":1`, `"row":0`, 1),
		strings.Replace(good, `"row":1`, `"row":1,"row":2`, 1),
		strings.Replace(good, `"batch_id":`, `"unknown":"TOP_SECRET","batch_id":`, 1),
		`{"batch_id":"no","rows":[]}`,
		good + `{}`,
	} {
		if _, err := DecodeImportSelection(strings.NewReader(input)); err == nil || strings.Contains(err.Error(), "TOP_SECRET") {
			t.Fatalf("invalid selection accepted or leaked: %v", err)
		}
	}
	selection.Rows = append(selection.Rows, selection.Rows[0])
	encoded, _ := json.Marshal(selection)
	if _, err := DecodeImportSelection(strings.NewReader(string(encoded))); err == nil {
		t.Fatal("duplicate row IDs accepted")
	}
}

func TestInventoryImportSelectionAcceptsExpandedCSVPreview(t *testing.T) {
	for _, tag := range []string{strings.Repeat("t", 2020), strings.Repeat("<", 2020)} {
		t.Run(fmt.Sprintf("escaped=%v", strings.Contains(tag, "<")), func(t *testing.T) {
			var input strings.Builder
			input.WriteString("name,host,port,user,tags\n")
			for i := range MaxImportRows {
				fmt.Fprintf(&input, "h%d,h%d.example,,root,\"[\"\"%s\"\"]\"\n", i, i, tag)
			}
			if input.Len() > MaxImportBytes {
				t.Fatalf("CSV fixture exceeds upload limit: %d", input.Len())
			}
			rows, err := ParseInventoryImport(strings.NewReader(input.String()), "csv")
			if err != nil {
				t.Fatal(err)
			}
			svc, _, _, _ := newTestService(&fakeRepo{}, nil)
			preview, err := NewCommandService(svc).PreviewImport(rows)
			if err != nil {
				t.Fatal(err)
			}
			selection := ImportSelection{BatchID: preview.BatchID}
			for _, row := range preview.Rows {
				if len(row.Errors) != 0 {
					t.Fatalf("fixture row rejected: %+v", row)
				}
				selection.Rows = append(selection.Rows, row.ImportEntry)
			}
			encoded, err := json.Marshal(selection)
			if err != nil || len(encoded) <= MaxImportBytes {
				t.Fatalf("fixture must expand beyond upload limit: bytes=%d err=%v", len(encoded), err)
			}
			decoded, err := DecodeImportSelection(strings.NewReader(string(encoded)))
			if err != nil || !reflect.DeepEqual(decoded, selection) {
				t.Fatalf("valid preview cannot be committed: rows=%d err=%v", len(decoded.Rows), err)
			}
			if result := NewCommandService(svc).CommitImport(decoded); result.Outcome != CommandOutcomeSuccess || len(result.Created) != MaxImportRows {
				t.Fatalf("valid decoded selection cannot be committed: outcome=%s created=%d", result.Outcome, len(result.Created))
			}
		})
	}
	if _, err := DecodeImportSelection(strings.NewReader(strings.Repeat(" ", maxImportSelectionBytes+1))); err == nil {
		t.Fatal("oversized selection accepted")
	}
}
