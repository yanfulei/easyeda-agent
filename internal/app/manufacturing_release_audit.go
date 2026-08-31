package app

import (
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

const releaseSnapshotSchema = "easyeda.pcb.manufacturing-snapshot/v1"

var releaseLCSCPartPattern = regexp.MustCompile(`(?i)^C[0-9]+$`)

type releaseManufacturingSnapshot struct {
	SHA256           string
	CanonicalJSON    []byte
	Components       []releaseComponent
	BOMReferences    []string
	CPLReferences    []string
	CopperLayerCount int
	NeedsPTH         bool
	NeedsNPTH        bool
	TopSMT           bool
	BottomSMT        bool
}

type releaseBOMInspection struct {
	References []string `json:"references"`
	Rows       int      `json:"rows"`
	Matched    bool     `json:"matched"`
}

type releaseCPLInspection struct {
	References []string `json:"references"`
	Excluded   []string `json:"excluded"`
	Rows       int      `json:"rows"`
	Unit       string   `json:"unit"`
	Matched    bool     `json:"matched"`
}

func parseReleaseManufacturingSnapshot(result map[string]any) (*releaseManufacturingSnapshot, error) {
	if result == nil {
		return nil, fmt.Errorf("manufacturing snapshot result is missing")
	}
	if strField(result, "schemaVersion") != releaseSnapshotSchema {
		return nil, fmt.Errorf("manufacturing snapshot schema is %q, want %q", strField(result, "schemaVersion"), releaseSnapshotSchema)
	}
	if complete, ok := result["complete"].(bool); !ok || !complete {
		return nil, fmt.Errorf("manufacturing snapshot is not marked complete")
	}
	for _, name := range []string{
		"components", "pads", "lines", "arcs", "polylines", "vias", "pours", "poured",
		"fills", "regions", "strings", "attributes", "images", "objects", "dimensions",
		"layers", "nets",
	} {
		if _, ok := result[name].([]any); !ok {
			return nil, fmt.Errorf("manufacturing snapshot %s inventory is missing or malformed", name)
		}
	}
	if _, ok := result["drcRules"].(map[string]any); !ok {
		return nil, fmt.Errorf("manufacturing snapshot DRC rules are missing or malformed")
	}
	copperLayers, ok := releaseExactInt(result["copperLayerCount"])
	if !ok || copperLayers < 2 || copperLayers%2 != 0 {
		return nil, fmt.Errorf("manufacturing snapshot copperLayerCount=%v is invalid", result["copperLayerCount"])
	}

	canonical, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("canonicalize manufacturing snapshot: %w", err)
	}
	rawComponents := result["components"].([]any)
	if len(rawComponents) == 0 {
		return nil, fmt.Errorf("PCB has no placed components")
	}
	components := make([]releaseComponent, 0, len(rawComponents))
	seen := map[string]bool{}
	for i, item := range rawComponents {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("manufacturing snapshot components[%d] is not an object", i)
		}
		primitiveID := strings.TrimSpace(strField(m, "primitiveId"))
		designator := strings.TrimSpace(strField(m, "designator"))
		if primitiveID == "" || designator == "" {
			return nil, fmt.Errorf("manufacturing snapshot components[%d] is missing primitiveId or designator", i)
		}
		ref := normalizeReleaseReference(designator)
		if seen[ref] {
			return nil, fmt.Errorf("duplicate PCB designator %q", designator)
		}
		seen[ref] = true
		layer, ok := releaseExactInt(m["layer"])
		if !ok || (layer != 1 && layer != 2) {
			return nil, fmt.Errorf("component %s has invalid placement layer %v", designator, m["layer"])
		}
		addIntoBOM, ok := m["addIntoBom"].(bool)
		if !ok {
			return nil, fmt.Errorf("component %s has no boolean addIntoBom", designator)
		}
		x, xOK := asFloatOK(m["x"])
		y, yOK := asFloatOK(m["y"])
		rotation, rotationOK := asFloatOK(m["rotation"])
		if !xOK || !yOK || !rotationOK || !releaseFinite(x) || !releaseFinite(y) || !releaseFinite(rotation) {
			return nil, fmt.Errorf("component %s has non-finite placement data", designator)
		}
		pads, ok := m["pads"].([]any)
		if !ok {
			return nil, fmt.Errorf("component %s has no complete pad inventory", designator)
		}
		isSMT := false
		for padIndex, rawPad := range pads {
			pad, ok := rawPad.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("component %s pad %d is malformed", designator, padIndex)
			}
			padLayer, ok := releaseExactInt(pad["layer"])
			if !ok {
				return nil, fmt.Errorf("component %s pad %d has invalid layer", designator, padIndex)
			}
			hole, hasHole := pad["hole"]
			if !hasHole {
				return nil, fmt.Errorf("component %s pad %d has no hole classification", designator, padIndex)
			}
			if (padLayer == 1 || padLayer == 2) && hole == nil {
				isSMT = true
			}
		}
		footprint := ""
		if fp, ok := m["footprint"].(map[string]any); ok {
			footprint = strings.TrimSpace(strField(fp, "name"))
		}
		components = append(components, releaseComponent{
			PrimitiveID:  primitiveID,
			Designator:   designator,
			Layer:        layer,
			AddIntoBOM:   addIntoBOM,
			Manufacturer: strings.TrimSpace(strField(m, "manufacturerId")),
			Supplier:     strings.TrimSpace(strField(m, "supplierId")),
			Footprint:    footprint,
			X:            x,
			Y:            y,
			Rotation:     rotation,
			SMT:          isSMT,
		})
	}
	sort.Slice(components, func(i, j int) bool {
		return normalizeReleaseReference(components[i].Designator) < normalizeReleaseReference(components[j].Designator)
	})

	snapshot := &releaseManufacturingSnapshot{
		SHA256: releaseSHA256(canonical), CanonicalJSON: canonical,
		Components: components, CopperLayerCount: copperLayers,
	}
	for _, component := range components {
		if !component.AddIntoBOM {
			continue
		}
		ref := normalizeReleaseReference(component.Designator)
		snapshot.BOMReferences = append(snapshot.BOMReferences, ref)
		if component.SMT {
			snapshot.CPLReferences = append(snapshot.CPLReferences, ref)
			if component.Layer == 1 {
				snapshot.TopSMT = true
			} else {
				snapshot.BottomSMT = true
			}
		}
	}
	if len(snapshot.BOMReferences) == 0 {
		return nil, fmt.Errorf("no component is enabled for BOM export")
	}
	// The connector currently emits a merged top-level pad inventory, but the
	// official SDK contract also exposes component pads only through each
	// component's nested `pads` field. Read both populations so a runtime/API
	// variation cannot make a live through-hole disappear from the release
	// drill requirements. Hole classification is boolean, so duplicate records
	// in a merged inventory are harmless here.
	allPads := make([]any, 0, len(result["pads"].([]any)))
	allPads = append(allPads, result["pads"].([]any)...)
	for componentIndex, item := range rawComponents {
		component, ok := item.(map[string]any)
		if !ok {
			// The component loop above already reports this with the more useful
			// indexed diagnostic; keep this guard for future refactors.
			return nil, fmt.Errorf("manufacturing snapshot components[%d] is not an object", componentIndex)
		}
		componentPads, ok := component["pads"].([]any)
		if !ok {
			return nil, fmt.Errorf("manufacturing snapshot components[%d] has no complete pad inventory", componentIndex)
		}
		allPads = append(allPads, componentPads...)
	}
	for i, item := range allPads {
		pad, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("manufacturing snapshot pads[%d] is malformed", i)
		}
		hole, present := pad["hole"]
		if !present || hole == nil {
			continue
		}
		metallized, ok := pad["metallization"].(bool)
		if !ok {
			return nil, fmt.Errorf("manufacturing snapshot pad %q has a hole but no metallization flag", strField(pad, "primitiveId"))
		}
		if metallized {
			snapshot.NeedsPTH = true
		} else {
			snapshot.NeedsNPTH = true
		}
	}
	if len(result["vias"].([]any)) != 0 {
		snapshot.NeedsPTH = true
	}
	return snapshot, nil
}

func releaseExactInt(value any) (int, bool) {
	n, ok := asFloatOK(value)
	if !ok || !releaseFinite(n) || math.Trunc(n) != n || n < math.MinInt32 || n > math.MaxInt32 {
		return 0, false
	}
	return int(n), true
}

func releaseFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

type releaseCSVEncoding int

const (
	releaseCSVUTF8 releaseCSVEncoding = iota
	releaseCSVUTF16LE
	releaseCSVUTF16BE
)

type releaseCSVTable struct {
	Encoding releaseCSVEncoding
	Comma    rune
	Records  [][]string
	Header   int
}

func parseReleaseCSVTable(data []byte) (*releaseCSVTable, error) {
	text, encoding, err := decodeReleaseCSVText(data)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, comma := range []rune{'\t', ',', ';'} {
		reader := csv.NewReader(strings.NewReader(text))
		reader.Comma = comma
		reader.FieldsPerRecord = -1
		// Keep empty fields in their declared column positions. EasyEDA's BOM
		// legitimately leaves Value blank for connectors/LEDs; csv.Reader's
		// TrimLeadingSpace treats the tab after that blank as whitespace and
		// collapses the field, shifting Manufacturer Part/Supplier Part left by
		// one column. Every consumer trims cells explicitly after parsing.
		reader.TrimLeadingSpace = false
		records, err := reader.ReadAll()
		if err != nil {
			lastErr = err
			continue
		}
		for rowIndex, row := range records {
			for _, field := range row {
				if releaseReferenceHeader(field) {
					return &releaseCSVTable{Encoding: encoding, Comma: comma, Records: records, Header: rowIndex}, nil
				}
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no Designator/Reference column found")
}

func decodeReleaseCSVText(data []byte) (string, releaseCSVEncoding, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return "", releaseCSVUTF8, fmt.Errorf("CSV is empty")
	}
	encoding := releaseCSVUTF8
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) {
		encoding = releaseCSVUTF16LE
		data = data[2:]
	} else if bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		encoding = releaseCSVUTF16BE
		data = data[2:]
	} else {
		data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
		if bytes.IndexByte(data, 0) >= 0 {
			return "", encoding, fmt.Errorf("CSV contains NUL bytes without a UTF-16 BOM")
		}
		return string(data), encoding, nil
	}
	if len(data)%2 != 0 {
		return "", encoding, fmt.Errorf("UTF-16 CSV has an odd byte length")
	}
	words := make([]uint16, len(data)/2)
	for i := range words {
		if encoding == releaseCSVUTF16LE {
			words[i] = binary.LittleEndian.Uint16(data[i*2:])
		} else {
			words[i] = binary.BigEndian.Uint16(data[i*2:])
		}
	}
	return string(utf16.Decode(words)), encoding, nil
}

func encodeReleaseCSVTable(table *releaseCSVTable, records [][]string) ([]byte, error) {
	if table == nil {
		return nil, fmt.Errorf("CSV table is nil")
	}
	var text bytes.Buffer
	writer := csv.NewWriter(&text)
	writer.Comma = table.Comma
	writer.UseCRLF = true
	if err := writer.WriteAll(records); err != nil {
		return nil, err
	}
	if err := writer.Error(); err != nil {
		return nil, err
	}
	if table.Encoding == releaseCSVUTF8 {
		return append([]byte{0xef, 0xbb, 0xbf}, text.Bytes()...), nil
	}
	words := utf16.Encode([]rune(text.String()))
	out := make([]byte, 2+len(words)*2)
	if table.Encoding == releaseCSVUTF16LE {
		out[0], out[1] = 0xff, 0xfe
		for i, word := range words {
			binary.LittleEndian.PutUint16(out[2+i*2:], word)
		}
	} else {
		out[0], out[1] = 0xfe, 0xff
		for i, word := range words {
			binary.BigEndian.PutUint16(out[2+i*2:], word)
		}
	}
	return out, nil
}

func releaseHeaderKey(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, strings.TrimPrefix(strings.TrimSpace(value), "\ufeff"))
}

func releaseColumn(header []string, aliases ...string) (int, error) {
	want := map[string]bool{}
	for _, alias := range aliases {
		want[releaseHeaderKey(alias)] = true
	}
	for index, value := range header {
		if want[releaseHeaderKey(value)] {
			return index, nil
		}
	}
	return -1, fmt.Errorf("CSV is missing required column %s", strings.Join(aliases, "/"))
}

func releaseCell(row []string, column int, label string) (string, error) {
	if column < 0 || column >= len(row) {
		return "", fmt.Errorf("CSV row has no %s column", label)
	}
	return strings.TrimSpace(row[column]), nil
}

func auditReleaseBOM(data []byte, components []releaseComponent) (releaseBOMInspection, error) {
	table, err := parseReleaseCSVTable(data)
	if err != nil {
		return releaseBOMInspection{}, err
	}
	header := table.Records[table.Header]
	refCol, err := releaseColumn(header, "Designator", "Reference", "RefDes", "位号")
	if err != nil {
		return releaseBOMInspection{}, err
	}
	qtyCol, err := releaseColumn(header, "Quantity", "Qty", "数量")
	if err != nil {
		return releaseBOMInspection{}, err
	}
	footprintCol, err := releaseColumn(header, "Footprint", "Package", "封装")
	if err != nil {
		return releaseBOMInspection{}, err
	}
	mpnCol, err := releaseColumn(header, "Manufacturer Part", "Manufacturer Part Number", "MPN", "制造商料号")
	if err != nil {
		return releaseBOMInspection{}, err
	}
	lcscCol, err := releaseColumn(header, "Supplier Part", "Supplier Part Number", "LCSC Part", "LCSC", "供应商料号", "立创商城编号")
	if err != nil {
		return releaseBOMInspection{}, err
	}

	expected := map[string]releaseComponent{}
	for _, component := range components {
		if !component.AddIntoBOM {
			continue
		}
		ref := normalizeReleaseReference(component.Designator)
		if component.Footprint == "" || component.Manufacturer == "" || component.Supplier == "" {
			return releaseBOMInspection{}, fmt.Errorf("component %s is not orderable: footprint, manufacturer part and LCSC part are all required", ref)
		}
		if !releaseLCSCPartPattern.MatchString(component.Supplier) {
			return releaseBOMInspection{}, fmt.Errorf("component %s has invalid LCSC part %q", ref, component.Supplier)
		}
		expected[ref] = component
	}
	seen := map[string]bool{}
	rows := 0
	for rowIndex, row := range table.Records[table.Header+1:] {
		if csvRowBlank(row) {
			continue
		}
		refs, err := releaseReferencesFromCell(row, refCol)
		if err != nil {
			return releaseBOMInspection{}, fmt.Errorf("BOM row %d: %w", table.Header+rowIndex+2, err)
		}
		qtyText, err := releaseCell(row, qtyCol, "quantity")
		if err != nil {
			return releaseBOMInspection{}, fmt.Errorf("BOM row %d: %w", table.Header+rowIndex+2, err)
		}
		quantity, err := strconv.Atoi(qtyText)
		if err != nil || quantity != len(refs) {
			return releaseBOMInspection{}, fmt.Errorf("BOM row %d quantity %q does not equal %d designator(s)", table.Header+rowIndex+2, qtyText, len(refs))
		}
		footprint, _ := releaseCell(row, footprintCol, "footprint")
		mpn, _ := releaseCell(row, mpnCol, "manufacturer part")
		lcsc, _ := releaseCell(row, lcscCol, "supplier part")
		for _, ref := range refs {
			component, ok := expected[ref]
			if !ok {
				return releaseBOMInspection{}, fmt.Errorf("BOM contains unexpected or DNP designator %s", ref)
			}
			if seen[ref] {
				return releaseBOMInspection{}, fmt.Errorf("BOM contains duplicate designator %s", ref)
			}
			seen[ref] = true
			if !strings.EqualFold(footprint, component.Footprint) {
				return releaseBOMInspection{}, fmt.Errorf("BOM %s footprint=%q, live PCB=%q", ref, footprint, component.Footprint)
			}
			if !strings.EqualFold(mpn, component.Manufacturer) {
				return releaseBOMInspection{}, fmt.Errorf("BOM %s manufacturer part=%q, live PCB=%q", ref, mpn, component.Manufacturer)
			}
			if !strings.EqualFold(lcsc, component.Supplier) {
				return releaseBOMInspection{}, fmt.Errorf("BOM %s LCSC part=%q, live PCB=%q", ref, lcsc, component.Supplier)
			}
		}
		rows++
	}
	actual := releaseSortedKeys(seen)
	want := releaseSortedKeysFromComponents(expected)
	if err := compareReleaseReferences("BOM", want, actual); err != nil {
		return releaseBOMInspection{}, err
	}
	return releaseBOMInspection{References: actual, Rows: rows, Matched: true}, nil
}

func auditAndFilterReleaseCPL(data []byte, components []releaseComponent) ([]byte, releaseCPLInspection, error) {
	table, err := parseReleaseCSVTable(data)
	if err != nil {
		return nil, releaseCPLInspection{}, err
	}
	header := table.Records[table.Header]
	refCol, err := releaseColumn(header, "Designator", "Reference", "RefDes", "位号")
	if err != nil {
		return nil, releaseCPLInspection{}, err
	}
	footprintCol, err := releaseColumn(header, "Footprint", "Package", "封装")
	if err != nil {
		return nil, releaseCPLInspection{}, err
	}
	xCol, err := releaseColumn(header, "Mid X", "Center X", "X", "中心X")
	if err != nil {
		return nil, releaseCPLInspection{}, err
	}
	yCol, err := releaseColumn(header, "Mid Y", "Center Y", "Y", "中心Y")
	if err != nil {
		return nil, releaseCPLInspection{}, err
	}
	layerCol, err := releaseColumn(header, "Layer", "Side", "面别")
	if err != nil {
		return nil, releaseCPLInspection{}, err
	}
	rotationCol, err := releaseColumn(header, "Rotation", "Rotate", "Angle", "旋转")
	if err != nil {
		return nil, releaseCPLInspection{}, err
	}
	smdCol, err := releaseColumn(header, "SMD", "SMT", "贴片")
	if err != nil {
		return nil, releaseCPLInspection{}, err
	}

	all := map[string]releaseComponent{}
	expected := map[string]releaseComponent{}
	for _, component := range components {
		ref := normalizeReleaseReference(component.Designator)
		all[ref] = component
		if component.AddIntoBOM && component.SMT {
			expected[ref] = component
		}
	}
	filtered := append([][]string(nil), table.Records[:table.Header+1]...)
	seen := map[string]bool{}
	excluded := map[string]bool{}
	for rowIndex, row := range table.Records[table.Header+1:] {
		if csvRowBlank(row) {
			continue
		}
		refs, err := releaseReferencesFromCell(row, refCol)
		if err != nil || len(refs) != 1 {
			return nil, releaseCPLInspection{}, fmt.Errorf("CPL row %d must contain exactly one designator", table.Header+rowIndex+2)
		}
		ref := refs[0]
		component, known := all[ref]
		if !known {
			return nil, releaseCPLInspection{}, fmt.Errorf("CPL contains unknown designator %s", ref)
		}
		smd, _ := releaseCell(row, smdCol, "SMD")
		isSMDRow := releaseYes(smd)
		wanted, isExpected := expected[ref]
		if !isExpected {
			if component.AddIntoBOM && !component.SMT && isSMDRow {
				return nil, releaseCPLInspection{}, fmt.Errorf("CPL marks through-hole/mechanical component %s as SMD", ref)
			}
			excluded[ref] = true
			continue
		}
		if seen[ref] {
			return nil, releaseCPLInspection{}, fmt.Errorf("CPL contains duplicate designator %s", ref)
		}
		if !isSMDRow {
			return nil, releaseCPLInspection{}, fmt.Errorf("CPL marks SMT component %s as non-SMD", ref)
		}
		footprint, _ := releaseCell(row, footprintCol, "footprint")
		if !strings.EqualFold(footprint, wanted.Footprint) {
			return nil, releaseCPLInspection{}, fmt.Errorf("CPL %s footprint=%q, live PCB=%q", ref, footprint, wanted.Footprint)
		}
		xText, _ := releaseCell(row, xCol, "Mid X")
		yText, _ := releaseCell(row, yCol, "Mid Y")
		xMM, err := releaseMillimeters(xText)
		if err != nil {
			return nil, releaseCPLInspection{}, fmt.Errorf("CPL %s Mid X: %w", ref, err)
		}
		yMM, err := releaseMillimeters(yText)
		if err != nil {
			return nil, releaseCPLInspection{}, fmt.Errorf("CPL %s Mid Y: %w", ref, err)
		}
		if math.Abs(xMM-wanted.X*0.0254) > 0.02 || math.Abs(yMM-wanted.Y*0.0254) > 0.02 {
			return nil, releaseCPLInspection{}, fmt.Errorf("CPL %s position=(%.4f,%.4f)mm, live PCB=(%.4f,%.4f)mm", ref, xMM, yMM, wanted.X*0.0254, wanted.Y*0.0254)
		}
		layer, _ := releaseCell(row, layerCol, "layer")
		if !releaseLayerMatches(layer, wanted.Layer) {
			return nil, releaseCPLInspection{}, fmt.Errorf("CPL %s layer=%q does not match live PCB layer %d", ref, layer, wanted.Layer)
		}
		rotationText, _ := releaseCell(row, rotationCol, "rotation")
		rotation, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(rotationText), "°"), 64)
		if err != nil || !releaseFinite(rotation) {
			return nil, releaseCPLInspection{}, fmt.Errorf("CPL %s has invalid rotation %q", ref, rotationText)
		}
		if releaseRotationDelta(rotation, wanted.Rotation) > 0.01 {
			return nil, releaseCPLInspection{}, fmt.Errorf("CPL %s rotation=%.4f°, live PCB=%.4f°", ref, rotation, wanted.Rotation)
		}
		seen[ref] = true
		filtered = append(filtered, row)
	}
	want := releaseSortedKeysFromComponents(expected)
	actual := releaseSortedKeys(seen)
	if err := compareReleaseReferences("CPL SMT population", want, actual); err != nil {
		return nil, releaseCPLInspection{}, err
	}
	filteredData, err := encodeReleaseCSVTable(table, filtered)
	if err != nil {
		return nil, releaseCPLInspection{}, fmt.Errorf("encode filtered CPL: %w", err)
	}
	return filteredData, releaseCPLInspection{
		References: actual,
		Excluded:   releaseSortedKeys(excluded),
		Rows:       len(actual),
		Unit:       "mm",
		Matched:    true,
	}, nil
}

func releaseRotationDelta(a, b float64) float64 {
	normalize := func(value float64) float64 {
		value = math.Mod(value, 360)
		if value < 0 {
			value += 360
		}
		return value
	}
	delta := math.Abs(normalize(a) - normalize(b))
	if delta > 180 {
		delta = 360 - delta
	}
	return delta
}

func releaseReferencesFromCell(row []string, column int) ([]string, error) {
	value, err := releaseCell(row, column, "reference")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	}) {
		if ref := normalizeReleaseReference(token); ref != "" {
			seen[ref] = true
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("reference column is empty")
	}
	return releaseSortedKeys(seen), nil
}

func releaseSortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func releaseSortedKeysFromComponents(values map[string]releaseComponent) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func releaseYes(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "yes", "y", "true", "1", "是":
		return true
	default:
		return false
	}
}

func releaseMillimeters(value string) (float64, error) {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < 3 || !strings.EqualFold(trimmed[len(trimmed)-2:], "mm") {
		return 0, fmt.Errorf("value %q is not explicitly in mm", value)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(trimmed[:len(trimmed)-2]), 64)
	if err != nil || !releaseFinite(n) {
		return 0, fmt.Errorf("value %q is not finite", value)
	}
	return n, nil
}

func releaseLayerMatches(value string, layer int) bool {
	normalized := releaseHeaderKey(value)
	if layer == 1 {
		return normalized == "t" || normalized == "top" || normalized == "toplayer" || normalized == "顶层"
	}
	return normalized == "b" || normalized == "bottom" || normalized == "bottomlayer" || normalized == "底层"
}
