package slik

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	maxZIPEntries   = 2048
	maxUncompressed = 512 << 20
	maxMember       = 256 << 20
	spreadsheetNS   = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	packageRelsNS   = "http://schemas.openxmlformats.org/package/2006/relationships"
	contentTypesNS  = "http://schemas.openxmlformats.org/package/2006/content-types"
)

var (
	cellReference = regexp.MustCompile(`^([A-Z]+)([1-9][0-9]*)$`)
	integerValue  = regexp.MustCompile(`^[0-9]{1,15}$`)
	valueElement  = regexp.MustCompile(`(?s)<v(?:\s[^>]*)?(?:/>|>.*?</v>)`)
	inlineElement = regexp.MustCompile(`(?s)<is(?:\s[^>]*)?(?:/>|>.*?</is>)`)
	typeAttribute = regexp.MustCompile(`\s+t=(?:"[^"]*"|'[^']*')`)
)

type Row struct {
	Number  int
	Account string
}
type Workbook struct {
	Sheet                                    string
	HeaderRow                                int
	AccountColumn, BalanceColumn, RateColumn string
	Rows                                     []Row
}
type Values struct{ Balance, Rate string }
type cell struct {
	Ref, Column, Type, Style, Value string
	Start, End                      int64
	Formula                         bool
}

func openWorkbook(filename string) (*zip.ReadCloser, map[string]*zip.File, error) {
	archive, err := zip.OpenReader(filename)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid XLSX ZIP: %w", err)
	}
	files := make(map[string]*zip.File, len(archive.File))
	var total uint64
	if len(archive.File) > maxZIPEntries {
		archive.Close()
		return nil, nil, errors.New("XLSX has too many ZIP entries")
	}
	for _, file := range archive.File {
		name := file.Name
		cleanName := strings.TrimSuffix(name, "/")
		if cleanName == "" || cleanName == ".." || strings.HasPrefix(cleanName, "/") || strings.Contains(cleanName, "\\") || strings.Contains(cleanName, ":") || path.Clean(cleanName) != cleanName || strings.HasPrefix(cleanName, "../") || strings.Contains(cleanName, "/../") || file.FileInfo().Mode()&os.ModeSymlink != 0 {
			archive.Close()
			return nil, nil, errors.New("unsafe XLSX ZIP entry")
		}
		if _, exists := files[name]; exists {
			archive.Close()
			return nil, nil, errors.New("duplicate XLSX ZIP entry")
		}
		total += file.UncompressedSize64
		if file.UncompressedSize64 > maxMember || total > maxUncompressed {
			archive.Close()
			return nil, nil, errors.New("XLSX expands beyond size limit")
		}
		if strings.Contains(strings.ToLower(name), "vbaproject") {
			archive.Close()
			return nil, nil, errors.New("VBA workbook is unsupported")
		}
		files[name] = file
	}
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels"} {
		if files[name] == nil {
			archive.Close()
			return nil, nil, fmt.Errorf("XLSX missing %s", name)
		}
	}
	if files["xl/vbaProject.bin"] != nil || files["EncryptedPackage"] != nil {
		archive.Close()
		return nil, nil, errors.New("macro or encrypted workbook is unsupported")
	}
	return archive, files, nil
}

func readPart(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxMember+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMember {
		return nil, errors.New("XLSX member exceeds size limit")
	}
	return data, nil
}

func validateRelationships(files map[string]*zip.File) error {
	for name, file := range files {
		if !strings.HasSuffix(name, ".rels") {
			continue
		}
		data, err := readPart(file)
		if err != nil {
			return err
		}
		var relationships struct {
			XMLName xml.Name
			Items   []struct {
				ID     string `xml:"Id,attr"`
				Type   string `xml:"Type,attr"`
				Target string `xml:"Target,attr"`
				Mode   string `xml:"TargetMode,attr"`
			} `xml:"Relationship"`
		}
		if err := xml.Unmarshal(data, &relationships); err != nil {
			return fmt.Errorf("malformed relationship XML %s: %w", name, err)
		}
		if relationships.XMLName.Space != packageRelsNS || relationships.XMLName.Local != "Relationships" {
			return fmt.Errorf("malformed relationship root %s", name)
		}
		base := ""
		if name != "_rels/.rels" {
			if !strings.Contains(name, "/_rels/") {
				return fmt.Errorf("malformed relationship path %s", name)
			}
			base = path.Dir(strings.TrimSuffix(strings.Replace(name, "/_rels/", "/", 1), ".rels"))
		}
		seen := make(map[string]bool)
		for _, rel := range relationships.Items {
			if rel.ID == "" || rel.Type == "" || rel.Target == "" || seen[rel.ID] {
				return fmt.Errorf("malformed relationship in %s", name)
			}
			seen[rel.ID] = true
			if rel.Mode == "External" {
				continue
			}
			if rel.Mode != "" {
				return fmt.Errorf("unsupported relationship mode in %s", name)
			}
			target, err := url.PathUnescape(rel.Target)
			if err != nil || strings.ContainsAny(target, "\\?#") {
				return fmt.Errorf("unsafe relationship target in %s", name)
			}
			targetBase := base
			if strings.HasPrefix(target, "/") {
				targetBase = ""
				target = strings.TrimPrefix(target, "/")
			}
			resolved := path.Clean(path.Join(targetBase, target))
			if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") || files[resolved] == nil || files[resolved].FileInfo().IsDir() {
				return fmt.Errorf("missing or unsafe relationship target in %s", name)
			}
		}
	}
	return nil
}

func sheetPaths(files map[string]*zip.File) ([]string, error) {
	contentTypes, err := readPart(files["[Content_Types].xml"])
	if err != nil {
		return nil, err
	}
	var types struct {
		XMLName   xml.Name
		Overrides []struct {
			PartName    string `xml:"PartName,attr"`
			ContentType string `xml:"ContentType,attr"`
		} `xml:"Override"`
	}
	if err := xml.Unmarshal(contentTypes, &types); err != nil {
		return nil, fmt.Errorf("invalid XLSX content types: %w", err)
	}
	if types.XMLName.Space != contentTypesNS || types.XMLName.Local != "Types" {
		return nil, errors.New("invalid XLSX content types root")
	}
	validMain := false
	for _, part := range types.Overrides {
		if part.PartName == "/xl/workbook.xml" && part.ContentType == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml" {
			validMain = true
		}
		if strings.Contains(part.ContentType, "macroEnabled") {
			return nil, errors.New("macro-enabled workbook is unsupported")
		}
	}
	if !validMain {
		return nil, errors.New("XLSX workbook content type is missing or unsupported")
	}
	rootRels, err := readPart(files["_rels/.rels"])
	if err != nil {
		return nil, err
	}
	var root struct {
		XMLName xml.Name
		Items   []struct {
			Type       string `xml:"Type,attr"`
			Target     string `xml:"Target,attr"`
			TargetMode string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(rootRels, &root); err != nil {
		return nil, fmt.Errorf("invalid package relationships: %w", err)
	}
	if root.XMLName.Space != packageRelsNS || root.XMLName.Local != "Relationships" {
		return nil, errors.New("invalid package relationships root")
	}
	validRoot := false
	for _, rel := range root.Items {
		if strings.HasSuffix(rel.Type, "/officeDocument") && rel.TargetMode == "" && (rel.Target == "xl/workbook.xml" || rel.Target == "/xl/workbook.xml") {
			validRoot = true
		}
	}
	if !validRoot {
		return nil, errors.New("XLSX package has no valid workbook relationship")
	}
	workbook, err := readPart(files["xl/workbook.xml"])
	if err != nil {
		return nil, err
	}
	relations, err := readPart(files["xl/_rels/workbook.xml.rels"])
	if err != nil {
		return nil, err
	}
	var sheets struct {
		XMLName xml.Name
		Sheets  []struct {
			ID string `xml:"id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(workbook, &sheets); err != nil {
		return nil, fmt.Errorf("invalid workbook XML: %w", err)
	}
	if sheets.XMLName.Space != spreadsheetNS || sheets.XMLName.Local != "workbook" {
		return nil, errors.New("invalid workbook XML root")
	}
	var rels struct {
		XMLName xml.Name
		Items   []struct {
			ID         string `xml:"Id,attr"`
			Type       string `xml:"Type,attr"`
			Target     string `xml:"Target,attr"`
			TargetMode string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(relations, &rels); err != nil {
		return nil, fmt.Errorf("invalid workbook relationships: %w", err)
	}
	if rels.XMLName.Space != packageRelsNS || rels.XMLName.Local != "Relationships" {
		return nil, errors.New("invalid workbook relationships root")
	}
	byID := make(map[string]string)
	seenIDs := make(map[string]bool)
	for _, rel := range rels.Items {
		if rel.ID == "" || seenIDs[rel.ID] {
			return nil, errors.New("duplicate or invalid workbook relationship")
		}
		seenIDs[rel.ID] = true
		if !strings.HasSuffix(rel.Type, "/worksheet") {
			continue
		}
		target, err := url.PathUnescape(rel.Target)
		if err != nil || rel.TargetMode != "" || target == "" || strings.ContainsAny(target, "\\?#") || strings.Contains("/"+target+"/", "/../") {
			return nil, errors.New("unsafe worksheet relationship")
		}
		memberTarget := strings.TrimPrefix(target, "/")
		if path.Clean(memberTarget) != memberTarget {
			return nil, errors.New("unsafe worksheet relationship")
		}
		name := path.Join("xl", memberTarget)
		if strings.HasPrefix(target, "/") {
			name = memberTarget
		}
		if !strings.HasPrefix(name, "xl/worksheets/") || files[name] == nil {
			return nil, errors.New("missing or unsafe worksheet target")
		}
		byID[rel.ID] = name
	}
	if len(sheets.Sheets) == 0 {
		return nil, errors.New("workbook contains no worksheets")
	}
	paths := make([]string, 0, len(sheets.Sheets))
	for _, sheet := range sheets.Sheets {
		name := byID[sheet.ID]
		if name == "" {
			return nil, errors.New("malformed worksheet relationship")
		}
		paths = append(paths, name)
	}
	return paths, nil
}

func sharedStrings(files map[string]*zip.File) ([]string, error) {
	file := files["xl/sharedStrings.xml"]
	if file == nil {
		return nil, nil
	}
	data, err := readPart(file)
	if err != nil {
		return nil, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var result []string
	var text strings.Builder
	inString, inText, inPhonetic := false, false, false
	rootSeen := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid shared strings XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if !rootSeen {
				if value.Name.Space != spreadsheetNS || value.Name.Local != "sst" {
					return nil, errors.New("invalid shared strings XML root")
				}
				rootSeen = true
			}
			if value.Name.Local == "si" {
				inString = true
				text.Reset()
			}
			if inString && value.Name.Local == "rPh" {
				inPhonetic = true
			}
			if inString && !inPhonetic && value.Name.Local == "t" {
				inText = true
			}
		case xml.CharData:
			if inText {
				text.Write(value)
			}
		case xml.EndElement:
			if value.Name.Local == "t" {
				inText = false
			}
			if value.Name.Local == "rPh" {
				inPhonetic = false
			}
			if value.Name.Local == "si" {
				result = append(result, text.String())
				inString = false
			}
		}
	}
	if !rootSeen {
		return nil, errors.New("empty shared strings XML")
	}
	return result, nil
}

func numberFormats(files map[string]*zip.File) ([]string, map[int]string, error) {
	file := files["xl/styles.xml"]
	if file == nil {
		return nil, nil, nil
	}
	data, err := readPart(file)
	if err != nil {
		return nil, nil, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	custom := make(map[int]string)
	var formats []string
	inXfs := false
	rootSeen := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("invalid styles XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if !rootSeen {
				if value.Name.Space != spreadsheetNS || value.Name.Local != "styleSheet" {
					return nil, nil, errors.New("invalid styles XML root")
				}
				rootSeen = true
			}
			switch value.Name.Local {
			case "cellXfs":
				inXfs = true
			case "numFmt":
				id, _ := strconv.Atoi(attribute(value, "numFmtId"))
				custom[id] = attribute(value, "formatCode")
			case "xf":
				if inXfs {
					formats = append(formats, attribute(value, "numFmtId"))
				}
			}
		case xml.EndElement:
			if value.Name.Local == "cellXfs" {
				inXfs = false
			}
		}
	}
	if !rootSeen {
		return nil, nil, errors.New("empty styles XML")
	}
	return formats, custom, nil
}

func attribute(element xml.StartElement, name string) string {
	for _, attr := range element.Attr {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

func column(ref string) string {
	match := cellReference.FindStringSubmatch(ref)
	if match == nil {
		return ""
	}
	return match[1]
}

func columnIndex(letters string) int {
	index := 0
	for _, letter := range letters {
		index = index*26 + int(letter-'A'+1)
	}
	return index
}

func walkSheet(data []byte, visit func(int, []cell, int64) error) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var row int
	lastRow := 0
	var cells []cell
	var current *cell
	var inValue, inText bool
	rootSeen := false
	for {
		start := decoder.InputOffset()
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid worksheet XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if !rootSeen {
				if value.Name.Space != spreadsheetNS || value.Name.Local != "worksheet" {
					return errors.New("invalid worksheet XML root")
				}
				rootSeen = true
			}
			switch value.Name.Local {
			case "row":
				row, err = strconv.Atoi(attribute(value, "r"))
				if err != nil || row < 1 || row > 1048576 || row <= lastRow {
					return errors.New("worksheet row lacks valid number")
				}
				lastRow = row
				cells = cells[:0]
			case "c":
				ref := attribute(value, "r")
				match := cellReference.FindStringSubmatch(ref)
				if match == nil || len(match[1]) > 3 || columnIndex(match[1]) > 16384 || match[2] != strconv.Itoa(row) {
					return errors.New("worksheet cell lacks valid reference")
				}
				current = &cell{Ref: ref, Column: match[1], Type: attribute(value, "t"), Style: attribute(value, "s"), Start: start}
			case "v":
				if current != nil {
					inValue = true
				}
			case "t":
				if current != nil && current.Type == "inlineStr" {
					inText = true
				}
			case "f":
				if current != nil {
					current.Formula = true
				}
			}
		case xml.CharData:
			if current != nil && (inValue || inText) {
				current.Value += string(value)
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "v":
				inValue = false
			case "t":
				inText = false
			case "c":
				if current != nil {
					current.End = decoder.InputOffset()
					cells = append(cells, *current)
					current = nil
				}
			case "row":
				seen := make(map[string]bool, len(cells))
				for _, cell := range cells {
					if seen[cell.Ref] {
						return errors.New("duplicate worksheet cell")
					}
					seen[cell.Ref] = true
				}
				if err := visit(row, cells, start); err != nil {
					return err
				}
			}
		}
	}
	if !rootSeen {
		return errors.New("empty worksheet XML")
	}
	return nil
}

func textValue(value cell, shared []string) (string, error) {
	if value.Type != "s" {
		return value.Value, nil
	}
	index, err := strconv.Atoi(strings.TrimSpace(value.Value))
	if err != nil || index < 0 || index >= len(shared) {
		return "", errors.New("invalid shared-string index")
	}
	return shared[index], nil
}

func accountValue(value cell, shared []string, formats []string, custom map[int]string) (string, error) {
	if value.Formula {
		return "", errors.New("formula account cell is unsupported")
	}
	if value.Type == "s" || value.Type == "inlineStr" || value.Type == "str" {
		result, err := textValue(value, shared)
		return strings.TrimSpace(result), err
	}
	if value.Type != "" && value.Type != "n" {
		return "", errors.New("unsupported account cell type")
	}
	if !integerValue.MatchString(value.Value) {
		return "", errors.New("ambiguous numeric account cell")
	}
	digits := strings.TrimLeft(value.Value, "0")
	if digits == "" {
		digits = "0"
	}
	style := 0
	if value.Style != "" {
		var err error
		style, err = strconv.Atoi(value.Style)
		if err != nil || style < 0 {
			return "", errors.New("invalid account cell style")
		}
	}
	if style >= len(formats) && value.Style != "" {
		return "", errors.New("missing account number format")
	}
	format := "General"
	if style < len(formats) {
		id, _ := strconv.Atoi(formats[style])
		if id != 0 {
			format = custom[id]
			if id == 1 {
				format = "0"
			}
		}
	}
	if format == "General" {
		return digits, nil
	}
	if format == "" || strings.Trim(format, "0") != "" {
		return "", errors.New("ambiguous numeric account format")
	}
	return strings.Repeat("0", max(0, len(format)-len(digits))) + digits, nil
}

func Inspect(filename, originalName string) (Workbook, error) {
	if !strings.EqualFold(path.Ext(originalName), ".xlsx") {
		return Workbook{}, errors.New("only .xlsx files are accepted")
	}
	archive, files, err := openWorkbook(filename)
	if err != nil {
		return Workbook{}, err
	}
	defer archive.Close()
	if err := validateRelationships(files); err != nil {
		return Workbook{}, err
	}
	var scannedTotal int64
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			return Workbook{}, err
		}
		limited := &io.LimitedReader{R: reader, N: maxMember + 1}
		if strings.HasSuffix(file.Name, ".xml") || strings.HasSuffix(file.Name, ".rels") {
			decoder := xml.NewDecoder(limited)
			for {
				_, err = decoder.Token()
				if err != nil {
					break
				}
			}
			if err == io.EOF {
				err = nil
			}
		} else {
			_, err = io.Copy(io.Discard, limited)
		}
		closeErr := reader.Close()
		scannedTotal += maxMember + 1 - limited.N
		if limited.N == 0 || scannedTotal > maxUncompressed {
			return Workbook{}, errors.New("XLSX expands beyond size limit")
		}
		if err != nil {
			return Workbook{}, fmt.Errorf("invalid XLSX member %s: %w", file.Name, err)
		}
		if closeErr != nil {
			return Workbook{}, closeErr
		}
	}
	paths, err := sheetPaths(files)
	if err != nil {
		return Workbook{}, err
	}
	shared, err := sharedStrings(files)
	if err != nil {
		return Workbook{}, err
	}
	formats, custom, err := numberFormats(files)
	if err != nil {
		return Workbook{}, err
	}
	var selected Workbook
	for _, name := range paths {
		data, err := readPart(files[name])
		if err != nil {
			return Workbook{}, err
		}
		var candidate Workbook
		err = walkSheet(data, func(row int, cells []cell, _ int64) error {
			headers := make(map[string]string, 3)
			duplicate := false
			for _, value := range cells {
				text, err := textValue(value, shared)
				if err != nil {
					return err
				}
				text = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "\ufeff")))
				if text == "nomorekeningfasilitas" || text == "bakidebet" || text == "sukubungaimbalan" {
					if headers[text] != "" {
						duplicate = true
					}
					headers[text] = value.Column
				}
			}
			if len(headers) == 3 {
				if duplicate {
					return errors.New("duplicate SLIK header")
				}
				if candidate.HeaderRow != 0 {
					return errors.New("multiple SLIK header rows")
				}
				candidate = Workbook{Sheet: name, HeaderRow: row, AccountColumn: headers["nomorekeningfasilitas"], BalanceColumn: headers["bakidebet"], RateColumn: headers["sukubungaimbalan"]}
			}
			return nil
		})
		if err != nil {
			return Workbook{}, err
		}
		if candidate.HeaderRow == 0 {
			continue
		}
		if selected.HeaderRow != 0 {
			return Workbook{}, errors.New("multiple worksheets contain SLIK headers")
		}
		selected = candidate
	}
	if selected.HeaderRow == 0 {
		return Workbook{}, errors.New("no worksheet contains all SLIK headers")
	}
	data, err := readPart(files[selected.Sheet])
	if err != nil {
		return Workbook{}, err
	}
	err = walkSheet(data, func(row int, cells []cell, _ int64) error {
		if row <= selected.HeaderRow {
			return nil
		}
		hasAccount := false
		for _, value := range cells {
			if value.Column != selected.AccountColumn {
				continue
			}
			account, err := accountValue(value, shared, formats, custom)
			if err != nil {
				return fmt.Errorf("row %d: %w", row, err)
			}
			if account != "" {
				selected.Rows = append(selected.Rows, Row{Number: row, Account: account})
				hasAccount = true
			}
			break
		}
		if !hasAccount {
			return nil
		}
		for _, value := range cells {
			if (value.Column == selected.BalanceColumn || value.Column == selected.RateColumn) && (value.Formula || (value.Type != "" && value.Type != "n" && value.Type != "s" && value.Type != "str" && value.Type != "inlineStr")) {
				return fmt.Errorf("row %d has an unsupported target cell", row)
			}
		}
		return nil
	})
	if err != nil {
		return Workbook{}, err
	}
	if len(selected.Rows) == 0 {
		return Workbook{}, errors.New("SLIK worksheet contains no account rows")
	}
	return selected, nil
}

type replacement struct {
	start, end int64
	data       string
}

func patchCell(data []byte, value cell, amount string) (string, error) {
	if value.Formula {
		return "", errors.New("target formula cell is unsupported")
	}
	raw := string(data[value.Start:value.End])
	if strings.HasSuffix(raw, "/>") {
		raw = strings.TrimSuffix(raw, "/>") + "></c>"
	}
	if value.Type == "s" || value.Type == "inlineStr" || value.Type == "str" {
		if value.Type == "s" || value.Type == "str" {
			end := strings.IndexByte(raw, '>')
			raw = typeAttribute.ReplaceAllString(raw[:end], ` t="inlineStr"`) + raw[end:]
		}
		newValue := "<is><t>" + amount + "</t></is>"
		if inlineElement.MatchString(raw) {
			return inlineElement.ReplaceAllString(raw, newValue), nil
		}
		if valueElement.MatchString(raw) {
			return valueElement.ReplaceAllString(raw, newValue), nil
		}
		return strings.Replace(raw, "</c>", newValue+"</c>", 1), nil
	}
	if value.Type != "" && value.Type != "n" {
		return "", errors.New("unsupported target cell type")
	}
	newValue := "<v>" + amount + "</v>"
	if valueElement.MatchString(raw) {
		return valueElement.ReplaceAllString(raw, newValue), nil
	}
	return strings.Replace(raw, "</c>", newValue+"</c>", 1), nil
}

func writeSheet(writer io.Writer, data []byte, workbook Workbook, results map[string]Values) error {
	byRow := make(map[int]string, len(workbook.Rows))
	for _, row := range workbook.Rows {
		byRow[row.Number] = row.Account
	}
	var changes []replacement
	err := walkSheet(data, func(row int, cells []cell, rowEnd int64) error {
		account := byRow[row]
		if account == "" {
			return nil
		}
		result, ok := results[account]
		if !ok {
			return fmt.Errorf("missing result for account row %d", row)
		}
		targets := []struct{ col, value string }{{workbook.BalanceColumn, result.Balance}, {workbook.RateColumn, result.Rate}}
		sort.Slice(targets, func(i, j int) bool { return columnIndex(targets[i].col) < columnIndex(targets[j].col) })
		for _, target := range targets {
			found := false
			for _, cell := range cells {
				if cell.Column == target.col {
					patched, err := patchCell(data, cell, target.value)
					if err != nil {
						return fmt.Errorf("row %d: %w", row, err)
					}
					changes = append(changes, replacement{cell.Start, cell.End, patched})
					found = true
					break
				}
			}
			if !found {
				position := rowEnd
				for _, existing := range cells {
					if columnIndex(existing.Column) > columnIndex(target.col) {
						position = existing.Start
						break
					}
				}
				changes = append(changes, replacement{position, position, `<c r="` + target.col + strconv.Itoa(row) + `"><v>` + target.value + `</v></c>`})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Cell changes remain ordered by source offsets. Insertions at one row end are stable.
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].start < changes[j].start })
	var offset int64
	for _, change := range changes {
		if change.start < offset {
			return errors.New("overlapping XLSX cell patches")
		}
		if _, err := writer.Write(data[offset:change.start]); err != nil {
			return err
		}
		if _, err := io.WriteString(writer, change.data); err != nil {
			return err
		}
		offset = change.end
	}
	_, err = writer.Write(data[offset:])
	return err
}

func Patch(input, output string, workbook Workbook, results map[string]Values) error {
	archive, files, err := openWorkbook(input)
	if err != nil {
		return err
	}
	defer archive.Close()
	sheet := files[workbook.Sheet]
	if sheet == nil {
		return errors.New("SLIK worksheet missing")
	}
	data, err := readPart(sheet)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(path.Dir(output), ".slik-output-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	if err := temporary.Chmod(0600); err != nil {
		return err
	}
	zipWriter := zip.NewWriter(temporary)
	for _, file := range archive.File {
		if file.Name != workbook.Sheet {
			if err := zipWriter.Copy(file); err != nil {
				return err
			}
			continue
		}
		part, err := zipWriter.CreateHeader(&file.FileHeader)
		if err != nil {
			return err
		}
		if err := writeSheet(part, data, workbook, results); err != nil {
			return err
		}
	}
	if err := zipWriter.Close(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if _, err := Inspect(temporary.Name(), "verified.xlsx"); err != nil {
		return fmt.Errorf("generated XLSX validation: %w", err)
	}
	if err := os.Rename(temporary.Name(), output); err != nil {
		return err
	}
	directory, err := os.Open(path.Dir(output))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
