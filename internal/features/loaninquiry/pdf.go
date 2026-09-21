package loaninquiry

import (
	"bytes"
	_ "embed"
	"fmt"
	"net/http"
	"strings"
	"time"

	"codeberg.org/go-pdf/fpdf"

	"github.com/ibldzn/trs/internal/loan"
)

//go:embed assets/logo.png
var bankLogo []byte

const (
	pdfLeftMargin   = 12.0
	pdfRightMargin  = 12.0
	pdfBottomMargin = 14.0
	pdfTableRow     = 6.2
)

type pdfDetail struct {
	label string
	value string
}

type pdfColumn struct {
	title string
	width float64
	align string
}

var scheduleColumns = []pdfColumn{
	{title: "NO.", width: 9, align: "C"},
	{title: "DATE", width: 23, align: "L"},
	{title: "INSTALLMENT", width: 34, align: "R"},
	{title: "PRINCIPAL", width: 31, align: "R"},
	{title: "INTEREST", width: 29, align: "R"},
	{title: "OUTSTANDING", width: 35, align: "R"},
	{title: "STATUS", width: 25, align: "C"},
}

func (handler *Handler) PDF(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	if len(query["account"]) != 1 || len(query["as_of"]) != 1 {
		http.Error(writer, "Account and reporting date are required.", http.StatusUnprocessableEntity)
		return
	}
	account := strings.TrimSpace(query.Get("account"))
	asOf, err := loan.ParseDate(strings.TrimSpace(query.Get("as_of")), handler.location)
	if account == "" || err != nil {
		http.Error(writer, "Account and reporting date are required.", http.StatusUnprocessableEntity)
		return
	}
	_, view, err := handler.resolveResult(request.Context(), account, asOf)
	if err != nil {
		status, message := inquiryError(err)
		http.Error(writer, message, status)
		return
	}
	printedAt := time.Now()
	if handler.location != nil {
		printedAt = printedAt.In(handler.location)
	}
	document, err := generateLoanPDF(view, printedAt)
	if err != nil {
		if handler.logger != nil {
			handler.logger.ErrorContext(request.Context(), "generate loan inquiry PDF", "error", err)
		}
		http.Error(writer, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	filename := fmt.Sprintf("jadwal-pembayaran-%s-%s.pdf", safeFilenamePart(account), asOf.String())
	writer.Header().Set("Content-Type", "application/pdf")
	writer.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, filename))
	writer.Header().Set("Content-Length", fmt.Sprint(len(document)))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(document)
}

func generateLoanPDF(view ResultView, printedAt time.Time) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(pdfLeftMargin, 12, pdfRightMargin)
	pdf.SetAutoPageBreak(false, pdfBottomMargin)
	pdf.SetCompression(false)
	pdf.SetTitle("Jadwal Pembayaran Kontraktual", false)
	pdf.SetAuthor("BANK DP TASPEN", false)
	pdf.SetCreator("new-trs", false)
	imageOptions := fpdf.ImageOptions{ImageType: "PNG", ReadDpi: true}
	pdf.RegisterImageOptionsReader("bank-dp-taspen-logo", imageOptions, bytes.NewReader(bankLogo))
	pdf.SetFooterFunc(func() {
		pdf.SetY(-9)
		pdf.SetFont("Helvetica", "", 7)
		pdf.SetTextColor(100, 116, 139)
		pdf.CellFormat(186, 4, fmt.Sprintf("BANK DP TASPEN  |  Page %d", pdf.PageNo()), "", 0, "R", false, 0, "")
	})
	pdf.AddPage()
	writePDFBrandHeader(pdf, imageOptions, view, printedAt)
	writePDFLoanInformation(pdf, view)
	writePDFScheduleTitle(pdf, false)
	writePDFScheduleHeader(pdf)
	for index, row := range view.ScheduleRows {
		_, pageHeight := pdf.GetPageSize()
		if pdf.GetY()+pdfTableRow > pageHeight-pdfBottomMargin {
			pdf.AddPage()
			writePDFContinuationHeader(pdf, imageOptions, view)
			writePDFScheduleTitle(pdf, true)
			writePDFScheduleHeader(pdf)
		}
		writePDFScheduleRow(pdf, row, index%2 == 1)
	}
	var output bytes.Buffer
	if err := pdf.Output(&output); err != nil {
		return nil, fmt.Errorf("render PDF: %w", err)
	}
	return output.Bytes(), nil
}

func writePDFBrandHeader(pdf *fpdf.Fpdf, imageOptions fpdf.ImageOptions, view ResultView, printedAt time.Time) {
	pdf.ImageOptions("bank-dp-taspen-logo", pdfLeftMargin, 12, 48, 0, false, imageOptions, 0, "")
	pdf.SetXY(66, 12)
	pdf.SetTextColor(30, 58, 138)
	pdf.SetFont("Helvetica", "B", 12)
	pdf.CellFormat(132, 6, "BANK DP TASPEN", "", 1, "R", false, 0, "")
	pdf.SetX(66)
	pdf.SetTextColor(15, 23, 42)
	pdf.SetFont("Helvetica", "B", 10)
	pdf.CellFormat(132, 6, "JADWAL PEMBAYARAN KONTRAKTUAL", "", 1, "R", false, 0, "")
	pdf.SetDrawColor(203, 213, 225)
	pdf.Line(pdfLeftMargin, 31, 198, 31)
	pdf.SetXY(pdfLeftMargin, 35)
	pdf.SetTextColor(71, 85, 105)
	pdf.SetFont("Helvetica", "B", 7.5)
	pdf.CellFormat(30, 5, "TANGGAL CETAK", "", 0, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 8)
	pdf.CellFormat(62, 5, printedAt.Format("02 Jan 2006 15:04 MST"), "", 0, "L", false, 0, "")
	pdf.SetFont("Helvetica", "B", 7.5)
	pdf.CellFormat(30, 5, "REPORTING DATE", "", 0, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 8)
	pdf.CellFormat(64, 5, formatPDFReportingDate(view), "", 1, "L", false, 0, "")
	pdf.Ln(3)
}

func writePDFContinuationHeader(pdf *fpdf.Fpdf, imageOptions fpdf.ImageOptions, view ResultView) {
	pdf.ImageOptions("bank-dp-taspen-logo", pdfLeftMargin, 10, 32, 0, false, imageOptions, 0, "")
	pdf.SetXY(50, 10)
	pdf.SetFont("Helvetica", "B", 8)
	pdf.SetTextColor(30, 58, 138)
	pdf.CellFormat(148, 5, "BANK DP TASPEN  |  JADWAL PEMBAYARAN KONTRAKTUAL", "", 1, "R", false, 0, "")
	pdf.SetX(50)
	pdf.SetFont("Helvetica", "", 7)
	pdf.SetTextColor(100, 116, 139)
	pdf.CellFormat(148, 4, view.PrimaryAccount+"  |  "+view.PeriodLabel, "", 1, "R", false, 0, "")
	pdf.SetDrawColor(203, 213, 225)
	pdf.Line(pdfLeftMargin, 22, 198, 22)
	pdf.SetY(25)
}

func writePDFLoanInformation(pdf *fpdf.Fpdf, view ResultView) {
	pdf.SetFillColor(241, 245, 249)
	pdf.SetTextColor(30, 41, 59)
	pdf.SetFont("Helvetica", "B", 9)
	pdf.CellFormat(186, 7, "INFORMASI PINJAMAN", "", 1, "L", true, 0, "")
	pdf.Ln(2)
	y := pdf.GetY()
	left := []pdfDetail{
		{"NAMA NASABAH", view.CustomerName},
		{"REKENING", view.PrimaryAccount},
		{"ALT REKENING", view.AlternateAccount},
		{"KANTOR CABANG", view.Branch},
		{"PRODUK", view.Product},
		{"PERIODE PINJAMAN", view.LoanPeriod},
		{"ANGSURAN", view.InstallmentSummary},
		{"DENDA PELUNASAN DIPERCEPAT", view.EarlyTerminationEstimate},
	}
	right := []pdfDetail{
		{"PLAFON AKAD", view.ContractPrincipal},
		{"SB EFEKTIF", view.ReferenceRate},
		{"SB KONTRAK", view.FlatRate},
		{"KOLEK", view.Collectability},
		{"BAKI DEBET", view.PrincipalOutstanding},
		{"TUNGGAKAN POKOK", view.PrincipalDue},
		{"TUNGGAKAN BUNGA", view.InterestDue},
		{"TUNGGAKAN PINALTI", view.PenaltyDue},
	}
	writePDFDetailColumn(pdf, pdfLeftMargin, y, left)
	writePDFDetailColumn(pdf, 107, y, right)
	pdf.SetY(y + float64(len(left))*5 + 4)
	pdf.SetFont("Helvetica", "I", 6.5)
	pdf.SetTextColor(100, 116, 139)
}

func writePDFDetailColumn(pdf *fpdf.Fpdf, x, y float64, details []pdfDetail) {
	for index, detail := range details {
		rowY := y + float64(index)*5
		pdf.SetXY(x, rowY)
		writeFittedCell(pdf, 38, 5, detail.label, "B", 6.4, "L", "", false)
		writeFittedCell(pdf, 53, 5, detail.value, "", 7.3, "L", "", false)
	}
}

func writePDFScheduleTitle(pdf *fpdf.Fpdf, continuation bool) {
	pdf.Ln(3)
	pdf.SetTextColor(30, 58, 138)
	pdf.SetFont("Helvetica", "B", 7)
	pdf.CellFormat(186, 4, "REPAYMENT PLAN", "", 1, "L", false, 0, "")
	pdf.SetTextColor(15, 23, 42)
	pdf.SetFont("Helvetica", "B", 10)
	title := "JADWAL PEMBAYARAN"
	if continuation {
		title += " (LANJUTAN)"
	}
	pdf.CellFormat(186, 6, title, "", 1, "L", false, 0, "")
	pdf.Ln(1)
}

func writePDFScheduleHeader(pdf *fpdf.Fpdf) {
	pdf.SetFillColor(30, 58, 138)
	pdf.SetDrawColor(203, 213, 225)
	pdf.SetTextColor(255, 255, 255)
	for _, column := range scheduleColumns {
		writeFittedCell(pdf, column.width, 7, column.title, "B", 6.8, column.align, "1", true)
	}
	pdf.Ln(-1)
}

func writePDFScheduleRow(pdf *fpdf.Fpdf, row ScheduleRowView, alternate bool) {
	values := []string{fmt.Sprint(row.Number), row.Date, row.Installment, row.Principal, row.Interest, row.Outstanding, row.Status}
	pdf.SetDrawColor(226, 232, 240)
	pdf.SetTextColor(30, 41, 59)
	if alternate {
		pdf.SetFillColor(248, 250, 252)
	} else {
		pdf.SetFillColor(255, 255, 255)
	}
	for index, column := range scheduleColumns {
		style := ""
		if index == 0 || index == 2 || index == 5 {
			style = "B"
		}
		writeFittedCell(pdf, column.width, pdfTableRow, values[index], style, 7, column.align, "1", true)
	}
	pdf.Ln(-1)
}

func writeFittedCell(pdf *fpdf.Fpdf, width, height float64, value, style string, size float64, align, border string, fill bool) {
	for pdf.SetFont("Helvetica", style, size); size > 5.2 && pdf.GetStringWidth(value) > width-1.5; size -= 0.2 {
		pdf.SetFont("Helvetica", style, size)
	}
	pdf.CellFormat(width, height, value, border, 0, align, fill, 0, "")
}

func formatPDFReportingDate(view ResultView) string {
	if strings.HasPrefix(view.PeriodLabel, "Periode ") {
		return strings.TrimPrefix(view.PeriodLabel, "Periode ")
	}
	return view.ReportingDate
}

func safeFilenamePart(value string) string {
	var filename strings.Builder
	for _, character := range strings.TrimSpace(value) {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			filename.WriteRune(character)
		} else if filename.Len() > 0 && !strings.HasSuffix(filename.String(), "-") {
			filename.WriteByte('-')
		}
	}
	if result := strings.Trim(filename.String(), "-"); result != "" {
		return result
	}
	return "account"
}
