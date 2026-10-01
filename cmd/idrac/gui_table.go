//go:build gui

package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// dataTable shows a captured CLI table with a header row, content-sized
// columns and single-row selection.
type dataTable struct {
	table    *widget.Table
	header   []string
	rows     [][]string
	selected int
	onSelect func(row int)
}

func newDataTable() *dataTable {
	d := &dataTable{selected: -1}
	d.table = widget.NewTable(
		func() (int, int) { return len(d.rows), len(d.header) },
		func() fyne.CanvasObject {
			l := widget.NewLabel("")
			l.Truncation = fyne.TextTruncateEllipsis
			return l
		},
		func(id widget.TableCellID, o fyne.CanvasObject) {
			l := o.(*widget.Label)
			if id.Row < len(d.rows) && id.Col < len(d.rows[id.Row]) {
				l.SetText(d.rows[id.Row][id.Col])
			} else {
				l.SetText("")
			}
		},
	)
	d.table.ShowHeaderRow = true
	d.table.CreateHeader = func() fyne.CanvasObject {
		return widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	}
	d.table.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) {
		l := o.(*widget.Label)
		if id.Col >= 0 && id.Col < len(d.header) {
			l.SetText(d.header[id.Col])
		} else {
			l.SetText("")
		}
	}
	d.table.OnSelected = func(id widget.TableCellID) {
		d.selected = id.Row
		if d.onSelect != nil {
			d.onSelect(id.Row)
		}
	}
	return d
}

// set replaces the contents and resizes the columns to fit.
func (d *dataTable) set(header []string, rows [][]string) {
	d.header, d.rows, d.selected = header, rows, -1
	d.table.UnselectAll()
	size := theme.TextSize()
	pad := theme.InnerPadding()*2 + 8
	for c := range header {
		w := fyne.MeasureText(header[c], size, fyne.TextStyle{Bold: true}).Width
		for i, r := range rows {
			if i >= 300 {
				break
			}
			if c < len(r) {
				if cw := fyne.MeasureText(r[c], size, fyne.TextStyle{}).Width; cw > w {
					w = cw
				}
			}
		}
		if w > 720 {
			w = 720
		}
		d.table.SetColumnWidth(c, w+pad)
	}
	d.table.Refresh()
}

// selectedRow returns the selected row's cells, or nil.
func (d *dataTable) selectedRow() []string {
	if d.selected < 0 || d.selected >= len(d.rows) {
		return nil
	}
	return d.rows[d.selected]
}
