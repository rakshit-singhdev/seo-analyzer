package main

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
)

// writeExcel saves the audit as a workbook with two sheets:
// Issues (one row per page to fix) and Pages (one row per crawled URL).
func writeExcel(path string, pages []*PageReport, issues []Issue) error {
	f := excelize.NewFile()
	defer f.Close()

	// A new workbook starts with one sheet named "Sheet1".
	if err := f.SetSheetName("Sheet1", "Issues"); err != nil {
		return err
	}
	if _, err := f.NewSheet("Pages"); err != nil {
		return err
	}

	// []any is a slice that can hold values of any type: strings, ints, etc.
	issueRows := [][]any{{"Page", "Issue", "Severity", "Detail", "Related URL or text"}}
	for _, issue := range issues {
		// On-page issues affect the Subject URL itself.
		if len(issue.Pages) == 0 {
			issueRows = append(issueRows, []any{issue.Subject, issue.Check, issue.Severity.String(), issue.Detail, ""})
			continue
		}
		// Link and duplicate issues get one row per page involved,
		// so the sheet can be filtered by the page someone needs to edit.
		for _, page := range issue.Pages {
			issueRows = append(issueRows, []any{page, issue.Check, issue.Severity.String(), issue.Detail, issue.Subject})
		}
	}

	pageRows := [][]any{{
		"URL", "Status", "Depth", "In sitemap", "Response (ms)", "Redirects to",
		"Title", "Title length", "Meta description", "Description length",
		"H1 count", "H1 text", "Canonical", "Internal links", "External links",
		"Images without alt", "Error",
	}}
	for _, r := range pages {
		pageRows = append(pageRows, []any{
			r.URL, r.StatusCode, r.Depth, r.InSitemap, r.ResponseTime.Milliseconds(), r.RedirectTo,
			r.Title, utf8.RuneCountInString(r.Title), r.MetaDescription, utf8.RuneCountInString(r.MetaDescription),
			len(r.H1s), strings.Join(r.H1s, " | "), r.Canonical, len(r.InternalLinks), len(r.ExternalLinks),
			r.ImagesMissingAlt, r.Error,
		})
	}

	if err := writeSheet(f, "Issues", issueRows, []float64{55, 22, 10, 45, 55}); err != nil {
		return err
	}
	pageWidths := []float64{55, 8, 7, 11, 13, 45, 45, 11, 55, 17, 9, 45, 45, 13, 13, 17, 30}
	if err := writeSheet(f, "Pages", pageRows, pageWidths); err != nil {
		return err
	}

	return f.SaveAs(path)
}

// writeSheet writes rows starting at A1, then makes the header row bold,
// freezes it so it stays visible while scrolling, adds filter buttons
// and sets column widths.
func writeSheet(f *excelize.File, sheet string, rows [][]any, widths []float64) error {
	for i, row := range rows {
		cell, err := excelize.CoordinatesToCellName(1, i+1) // column 1, row i+1 -> "A1", "A2", ...
		if err != nil {
			return err
		}
		if err := f.SetSheetRow(sheet, cell, &row); err != nil {
			return err
		}
	}

	lastCol, err := excelize.ColumnNumberToName(len(rows[0])) // e.g. 5 -> "E"
	if err != nil {
		return err
	}

	header, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"1F4E78"}},
	})
	if err != nil {
		return err
	}
	if err := f.SetCellStyle(sheet, "A1", lastCol+"1", header); err != nil {
		return err
	}

	if err := f.SetPanes(sheet, &excelize.Panes{
		Freeze:      true,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	}); err != nil {
		return err
	}

	filterRange := fmt.Sprintf("A1:%s%d", lastCol, len(rows))
	if err := f.AutoFilter(sheet, filterRange, nil); err != nil {
		return err
	}

	for i, width := range widths {
		col, err := excelize.ColumnNumberToName(i + 1)
		if err != nil {
			return err
		}
		if err := f.SetColWidth(sheet, col, col, width); err != nil {
			return err
		}
	}
	return nil
}