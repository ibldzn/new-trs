package slik

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureSheet = `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheetViews><sheetView workbookViewId="0"><pane xSplit="1" ySplit="1" topLeftCell="B2" state="frozen"/></sheetView></sheetViews><cols><col min="5" max="5" hidden="1" width="15"/></cols><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c><c r="D1" t="s"><v>5</v></c></row><row r="2"><c r="A2" s="1" t="s"><v>3</v></c><c r="B2" s="2" t="s"><v>6</v></c><c r="C2" s="2" t="s"><v>6</v></c><c r="D2" s="3"><f>1+1</f><v>2</v></c><c r="E2" t="s"><v>6</v></c></row><row r="3" hidden="1"><c r="A3" t="inlineStr"><is><t>OTHER</t></is></c><c r="B3" s="2"><v>0</v></c><c r="C3" s="2"><v>0</v></c></row><row r="4"><c r="A4" s="1"><v>123</v></c><c r="B4" s="2" t="s"><v>6</v></c><c r="C4" s="2" t="s"><v>6</v></c></row></sheetData><mergeCells count="1"><mergeCell ref="D3:E3"/></mergeCells><dataValidations count="1"><dataValidation sqref="E2" type="list"><formula1>"a,b"</formula1></dataValidation></dataValidations><hyperlinks><hyperlink ref="E2" r:id="rId9"/></hyperlinks><drawing r:id="rId10"/><pageMargins left="0.7" right="0.7" top="0.75" bottom="0.75" header="0.3" footer="0.3"/></worksheet>`

func fixture(t testing.TB, sheets int, header string, duplicate ...bool) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "input.xlsx")
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	if header == "" {
		header = "\ufeff NoMoReKeNiNgFaSiLiTaS "
	}
	parts := map[string]string{
		"[Content_Types].xml":                 `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/></Types>`,
		"_rels/.rels":                         `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`,
		"xl/sharedStrings.xml":                `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>` + header + `</t></si><si><t> Bakidebet </t></si><si><t>SUKUBUNGAIMBALAN</t></si><si><t>00123</t></si><si><t>OTHER</t></si><si><t>unrelated</t></si><si><t>old</t></si></sst>`,
		"xl/styles.xml":                       `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><numFmts count="1"><numFmt numFmtId="164" formatCode="00000"/></numFmts><fonts count="1"><font><b/><color rgb="FF113355"/></font></fonts><fills count="1"><fill><patternFill patternType="none"/></fill></fills><borders count="1"><border><left/><right/></border></borders><cellXfs count="4"><xf numFmtId="0"/><xf numFmtId="164"/><xf numFmtId="2"/><xf numFmtId="0"/></cellXfs></styleSheet>`,
		"xl/worksheets/sheet1.xml":            fixtureSheet,
		"xl/worksheets/_rels/sheet1.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId9" Target="https://example.invalid" TargetMode="External" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink"/><Relationship Id="rId10" Target="../drawings/drawing1.xml" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/drawing"/></Relationships>`,
		"xl/drawings/drawing1.xml":            `<drawing>image preserved</drawing>`,
		"xl/media/image1.png":                 "fake image bytes",
		"docProps/core.xml":                   `<core>metadata preserved</core>`,
	}
	parts["xl/worksheets/sheet1.xml"] = strings.Replace(parts["xl/worksheets/sheet1.xml"], `</sheetData><mergeCells`, `</sheetData><autoFilter ref="A1:E4"/><mergeCells`, 1)
	parts["xl/worksheets/sheet1.xml"] = strings.Replace(parts["xl/worksheets/sheet1.xml"], `</mergeCells><dataValidations`, `</mergeCells><conditionalFormatting sqref="D2"><cfRule type="expression" priority="1"><formula>D2&gt;0</formula></cfRule></conditionalFormatting><dataValidations`, 1)
	parts["xl/worksheets/sheet1.xml"] = strings.Replace(parts["xl/worksheets/sheet1.xml"], `</hyperlinks><drawing r:id="rId10"/><pageMargins`, `</hyperlinks><printOptions headings="1"/><pageMargins`, 1)
	parts["xl/worksheets/sheet1.xml"] = strings.Replace(parts["xl/worksheets/sheet1.xml"], `footer="0.3"/></worksheet>`, `footer="0.3"/><drawing r:id="rId10"/></worksheet>`, 1)
	var sheetTags, relationTags strings.Builder
	for i := 1; i <= sheets; i++ {
		sheetTags.WriteString(fmt.Sprintf(`<sheet name="Sheet %d" sheetId="%d" state="%s" r:id="rId%d"/>`, i, i, map[bool]string{true: "hidden", false: "visible"}[i > 1], i))
		relationTags.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i))
		if i > 1 {
			content := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData/></worksheet>`
			if len(duplicate) > 0 && duplicate[0] {
				content = fixtureSheet
			}
			parts[fmt.Sprintf("xl/worksheets/sheet%d.xml", i)] = content
		}
	}
	parts["xl/workbook.xml"] = `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` + sheetTags.String() + `</sheets><definedNames><definedName name="Print_Area">Sheet1!$A$1:$E$4</definedName></definedNames></workbook>`
	parts["xl/_rels/workbook.xml.rels"] = `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + relationTags.String() + `</Relationships>`
	for name, data := range parts {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestInspectAndPatchPreservesWorkbook(t *testing.T) {
	input := fixture(t, 2, "")
	workbook, err := Inspect(input, "input.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	if workbook.Sheet != "xl/worksheets/sheet1.xml" || workbook.HeaderRow != 1 || len(workbook.Rows) != 3 || workbook.Rows[0].Account != "00123" || workbook.Rows[2].Account != "00123" {
		t.Fatalf("workbook = %+v", workbook)
	}
	output := filepath.Join(t.TempDir(), "output.xlsx")
	values := map[string]Values{"00123": {Balance: "1234.56", Rate: "11.234567890123"}, "OTHER": {Balance: "0.00", Rate: "7.5"}}
	if err := Patch(input, output, workbook, values); err != nil {
		t.Fatal(err)
	}
	updated, err := Inspect(output, "output.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Rows) != 3 || updated.Rows[2].Account != "00123" {
		t.Fatalf("output accounts = %+v", updated.Rows)
	}
	before, err := zip.OpenReader(input)
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	after, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	byName := make(map[string]*zip.File)
	for _, file := range after.File {
		byName[file.Name] = file
	}
	for _, file := range before.File {
		other := byName[file.Name]
		if other == nil {
			t.Fatalf("missing ZIP member %s", file.Name)
		}
		original, err := readPart(file)
		if err != nil {
			t.Fatal(err)
		}
		generated, err := readPart(other)
		if err != nil {
			t.Fatal(err)
		}
		if file.Name == workbook.Sheet {
			if !bytes.Equal(maskTargets(t, original), maskTargets(t, generated)) {
				t.Fatal("unrelated worksheet XML changed")
			}
			for _, marker := range []string{`s="2" t="inlineStr"><is><t>1234.56</t></is>`, `s="2" t="inlineStr"><is><t>11.234567890123</t></is>`, `<f>1+1</f>`} {
				if !bytes.Contains(generated, []byte(marker)) {
					t.Fatalf("missing %s", marker)
				}
			}
		} else if !bytes.Equal(original, generated) {
			t.Fatalf("unrelated ZIP member changed: %s", file.Name)
		} else if !bytes.Equal(rawCompressed(t, input, file), rawCompressed(t, output, other)) {
			t.Fatalf("unrelated compressed ZIP member changed: %s", file.Name)
		}
	}
}

func rawCompressed(t *testing.T, filename string, part *zip.File) []byte {
	t.Helper()
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	offset, err := part.DataOffset()
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, part.CompressedSize64)
	if _, err := file.ReadAt(data, offset); err != nil {
		t.Fatal(err)
	}
	return data
}

func maskTargets(t *testing.T, data []byte) []byte {
	t.Helper()
	var changes []replacement
	err := walkSheet(data, func(row int, cells []cell, _ int64) error {
		if row < 2 {
			return nil
		}
		for _, cell := range cells {
			if cell.Column == "B" || cell.Column == "C" {
				changes = append(changes, replacement{cell.Start, cell.End, "<target/>"})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var result bytes.Buffer
	var offset int64
	for _, change := range changes {
		result.Write(data[offset:change.start])
		result.WriteString(change.data)
		offset = change.end
	}
	result.Write(data[offset:])
	return result.Bytes()
}

func TestWorkbookValidation(t *testing.T) {
	for _, extension := range []string{"file.csv", "file.xls", "file.xlsb", "file.xlsm"} {
		t.Run(extension, func(t *testing.T) {
			if _, err := Inspect(fixture(t, 1, ""), extension); err == nil {
				t.Fatal("accepted unsupported extension")
			}
		})
	}
	if _, err := Inspect(fixture(t, 1, "account"), "input.xlsx"); err == nil || !strings.Contains(err.Error(), "no worksheet") {
		t.Fatalf("zero sheets: %v", err)
	}
	if _, err := Inspect(fixture(t, 2, "", true), "input.xlsx"); err == nil || !strings.Contains(err.Error(), "multiple worksheets") {
		t.Fatalf("multiple sheets: %v", err)
	}
	input := fixture(t, 1, "")
	archive, err := zip.OpenReader(input)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if _, err := accountValue(cell{Type: "n", Value: "1.23E+15"}, nil, nil, nil); err == nil {
		t.Fatal("accepted ambiguous numeric account")
	}
	if got, err := accountValue(cell{Type: "n", Value: "000123"}, nil, nil, nil); err != nil || got != "123" {
		t.Fatalf("General numeric display=%q err=%v", got, err)
	}
	if _, err := accountValue(cell{Type: "n", Value: "123", Style: "-1"}, nil, []string{"0"}, nil); err == nil {
		t.Fatal("accepted negative style index")
	}
	if _, err := Inspect(fixture(t, 1, "  \ufeff  NoMoReKeNiNgFaSiLiTaS  "), "input.xlsx"); err != nil {
		t.Fatalf("BOM/whitespace header rejected: %v", err)
	}
}

func rewriteFixturePart(t *testing.T, input, member, old, replacement string) string {
	t.Helper()
	source, err := zip.OpenReader(input)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	output := filepath.Join(t.TempDir(), "rewritten.xlsx")
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	found := false
	for _, part := range source.File {
		data, err := readPart(part)
		if err != nil {
			t.Fatal(err)
		}
		if part.Name == member {
			if !strings.Contains(string(data), old) {
				t.Fatalf("missing replacement text in %s", member)
			}
			data = []byte(strings.Replace(string(data), old, replacement, 1))
			found = true
		}
		entry, err := writer.Create(part.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Fatalf("missing member %s", member)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestRejectsMacroAndMalformedRelationships(t *testing.T) {
	base := fixture(t, 1, "")
	macro := rewriteFixturePart(t, base, "[Content_Types].xml", ".sheet.main+xml", ".sheet.macroEnabled.main+xml")
	if _, err := Inspect(macro, "renamed.xlsx"); err == nil || !strings.Contains(err.Error(), "macro") {
		t.Fatalf("macro workbook accepted: %v", err)
	}
	badRel := rewriteFixturePart(t, base, "xl/_rels/workbook.xml.rels", `Target="worksheets/sheet1.xml"`, `Target="../outside.xml"`)
	if _, err := Inspect(badRel, "bad.xlsx"); err == nil || !strings.Contains(err.Error(), "unsafe relationship") {
		t.Fatalf("unsafe relationship accepted: %v", err)
	}
	badXML := rewriteFixturePart(t, base, "xl/worksheets/sheet1.xml", `</worksheet>`, `</broken>`)
	if _, err := Inspect(badXML, "bad.xlsx"); err == nil {
		t.Fatal("malformed worksheet XML accepted")
	}
	badUnrelatedXML := rewriteFixturePart(t, base, "docProps/core.xml", `</core>`, `</broken>`)
	if _, err := Inspect(badUnrelatedXML, "bad.xlsx"); err == nil {
		t.Fatal("malformed unrelated XML accepted")
	}
}

func TestRejectsTechnicalZIPLimits(t *testing.T) {
	for _, test := range []struct {
		name       string
		entries    int
		memberSize uint64
	}{
		{"entries", maxZIPEntries + 1, 0},
		{"member", 1, maxMember + 1},
		{"total", 3, 200 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "oversize.xlsx")
			file, err := os.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			writer := zip.NewWriter(file)
			for i := 0; i < test.entries; i++ {
				header := &zip.FileHeader{Name: fmt.Sprintf("member-%d.xml", i), Method: zip.Store}
				if test.memberSize > 0 {
					header.UncompressedSize64 = test.memberSize
				}
				if _, err := writer.CreateRaw(header); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			file.Close()
			if _, err := Inspect(name, "oversize.xlsx"); err == nil {
				t.Fatal("resource limit bypassed")
			}
		})
	}
}

func TestPatchAddsMissingTargetCellsInColumnOrder(t *testing.T) {
	data := []byte(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="2"><c r="A2" t="inlineStr"><is><t>A</t></is></c><c r="D2"><v>9</v></c></row></sheetData></worksheet>`)
	workbook := Workbook{AccountColumn: "A", BalanceColumn: "C", RateColumn: "B", Rows: []Row{{Number: 2, Account: "A"}}}
	var output bytes.Buffer
	if err := writeSheet(&output, data, workbook, map[string]Values{"A": {Balance: "12.34", Rate: "7.5"}}); err != nil {
		t.Fatal(err)
	}
	want := `<c r="A2" t="inlineStr"><is><t>A</t></is></c><c r="B2"><v>7.5</v></c><c r="C2"><v>12.34</v></c><c r="D2"><v>9</v></c>`
	if !strings.Contains(output.String(), want) {
		t.Fatalf("cell order: %s", output.String())
	}
}

func TestPatchReplacesSelfClosingTargetValue(t *testing.T) {
	data := []byte(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="2"><c r="A2" t="inlineStr"><is><t>A</t></is></c><c r="B2" s="2" t="s"><v/></c><c r="C2"><v/></c></row></sheetData></worksheet>`)
	workbook := Workbook{AccountColumn: "A", BalanceColumn: "B", RateColumn: "C", Rows: []Row{{Number: 2, Account: "A"}}}
	var output bytes.Buffer
	if err := writeSheet(&output, data, workbook, map[string]Values{"A": {Balance: "12.34", Rate: "7.5"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "<v/>") || !strings.Contains(output.String(), `<c r="B2" s="2" t="inlineStr"><is><t>12.34</t></is></c>`) || !strings.Contains(output.String(), `<c r="C2"><v>7.5</v></c>`) {
		t.Fatalf("self-closing value patch: %s", output.String())
	}
}

func TestBlankAccountRowKeepsTargetFormula(t *testing.T) {
	input := rewriteFixturePart(t, fixture(t, 1, ""), "xl/worksheets/sheet1.xml", `<c r="A3" t="inlineStr"><is><t>OTHER</t></is></c>`, `<c r="A3" t="inlineStr"><is><t></t></is></c>`)
	input = rewriteFixturePart(t, input, "xl/worksheets/sheet1.xml", `<c r="B3" s="2"><v>0</v></c>`, `<c r="B3" s="2"><f>1+1</f><v>2</v></c>`)
	workbook, err := Inspect(input, "input.xlsx")
	if err != nil || len(workbook.Rows) != 2 {
		t.Fatalf("workbook=%+v err=%v", workbook, err)
	}
	output := filepath.Join(t.TempDir(), "output.xlsx")
	if err := Patch(input, output, workbook, map[string]Values{"00123": {Balance: "12.34", Rate: "7.5"}}); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, part := range archive.File {
		if part.Name == workbook.Sheet {
			data, err := readPart(part)
			if err != nil || !bytes.Contains(data, []byte(`<c r="B3" s="2"><f>1+1</f><v>2</v></c>`)) {
				t.Fatalf("blank row formula changed: %v", err)
			}
		}
	}
}

func TestRejectsUnsafeZIPEntry(t *testing.T) {
	name := filepath.Join(t.TempDir(), "bad.xlsx")
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	part, err := writer.Create("../escape.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("x"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := Inspect(name, "bad.xlsx"); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe ZIP accepted: %v", err)
	}
}

func TestProvidedSLIKSample(t *testing.T) {
	input := filepath.Join("..", "..", "sample row slik.xlsx")
	if _, err := os.Stat(input); err != nil {
		t.Skip("provided sample is not present")
	}
	workbook, err := Inspect(input, "sample row slik.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	if len(workbook.Rows) != 1 || workbook.Rows[0].Account == "" {
		t.Fatalf("sample rows: %+v", workbook.Rows)
	}
	output := filepath.Join(t.TempDir(), "sample-output.xlsx")
	if err := Patch(input, output, workbook, map[string]Values{workbook.Rows[0].Account: {Balance: "1234.56", Rate: "11.234567890123"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(output, "sample-output.xlsx"); err != nil {
		t.Fatal(err)
	}
}
