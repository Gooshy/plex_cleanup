package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

// File extension definitions
var (
	// Extensions to remove (unwanted files)
	unwantedExtensions = []string{
		".rar", ".zip", ".7z", ".sfv", ".idx", ".nfo", ".txt",
		".par", ".par2", ".jpg", ".jpeg", ".png", ".gif",
	}

	// Safe extensions (media files that should NOT be deleted)
	safeExtensions = []string{
		".mp4", ".mkv", ".avi", ".mov", ".wmv", ".m4v", ".mpg", ".mpeg",
		".flv", ".vob", ".webm", ".divx", ".3gp", ".h264", ".h265",
	}

	// Patterns for file matching
	numberedPattern = regexp.MustCompile(`\.\d{3}$`)           // Matches .001, .002, etc.
	rarPartPattern  = regexp.MustCompile(`-\.r\d{2}$`)         // Matches -.r08, -.r09, etc. (exact format from image)
	partPattern     = regexp.MustCompile(`\.part\d+$`)         // Matches .part1, .part2, etc.
)

// FileInfo struct for tracking unwanted files
type FileInfo struct {
	Path string
	Size int64
}

// Extension statistics
type ExtStats struct {
	Count int
	Size  int64
}

// LiveStats holds the real-time scan statistics
type LiveStats struct {
	ExtensionStats map[string]*ExtStats
	TotalFiles     int
	TotalSize      int64
	FilesScanned   int
	DirsScanned    int
	mutex          sync.Mutex
}

// Format bytes to human-readable size
func formatSize(sizeBytes int64) string {
	const unit = 1024
	if sizeBytes < unit {
		return fmt.Sprintf("%d B", sizeBytes)
	}
	div, exp := int64(unit), 0
	for n := sizeBytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(sizeBytes)/float64(div), "KMGTPE"[exp])
}

// Get file extension for categorization
func getFileExtension(filename string) string {
	lowername := strings.ToLower(filename)
	
	// Special handling for RAR parts (like those in the image)
	if rarPartPattern.MatchString(lowername) {
		return ".rXX"  // Group all RAR parts together
	}
	
	// Special handling for numbered files
	if numberedPattern.MatchString(lowername) {
		return ".numbered"
	}
	
	return strings.ToLower(filepath.Ext(filename))
}

// Check if a file is unwanted
func isUnwantedFile(filename string) bool {
	lowername := strings.ToLower(filename)

	// Never delete files with safe extensions
	for _, ext := range safeExtensions {
		if strings.HasSuffix(lowername, ext) {
			return false
		}
	}

	// Check if file has an unwanted extension
	for _, ext := range unwantedExtensions {
		if strings.HasSuffix(lowername, ext) {
			return true
		}
	}

	// Check if file matches RAR part pattern (-.r08, -.r09, etc.)
	if rarPartPattern.MatchString(lowername) {
		return true
	}

	// Check if file matches a pattern (like .001, .002, etc.)
	if numberedPattern.MatchString(lowername) || partPattern.MatchString(lowername) {
		return true
	}

	return false
}

// FileTypeItem represents a row in the table
type FileTypeItem struct {
	FileType  string
	Count     int
	TotalSize string
	IsTotal   bool
}

// FileTypeTableModel implements walk.TableModel
type FileTypeTableModel struct {
	walk.TableModelBase
	items []FileTypeItem
}

func (m *FileTypeTableModel) RowCount() int {
	return len(m.items)
}

func (m *FileTypeTableModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return nil
	}

	item := m.items[row]

	switch col {
	case 0:
		return item.FileType
	case 1:
		return item.Count
	case 2:
		return item.TotalSize
	}

	return nil
}

// Delete files with cancellation support
func deleteFiles(ctx context.Context, files []FileInfo, progressBar *walk.ProgressBar) (int, int64) {
	deletedCount := 0
	var deletedSize int64

	progressBar.SetRange(0, len(files))

	for i, file := range files {
		// Check for cancellation
		select {
		case <-ctx.Done():
			return deletedCount, deletedSize
		default:
			// Continue processing
		}

		err := os.Remove(file.Path)
		if err != nil {
			log.Printf("Error deleting %s: %v\n", file.Path, err)
			continue
		}

		log.Printf("Deleted: %s (%s)\n", file.Path, formatSize(file.Size))
		deletedCount++
		deletedSize += file.Size
		progressBar.SetValue(i + 1)
	}
	return deletedCount, deletedSize
}

func main() {
	// Set up logging
	logFile := fmt.Sprintf("plex_cleanup_%s.log", time.Now().Format("20060102_150405"))
	f, err := os.Create(logFile)
	if err != nil {
		walk.MsgBox(nil, "Error", "Failed to create log file", walk.MsgBoxIconError)
		return
	}
	defer f.Close()
	log.SetOutput(f)

	var mainWindow *walk.MainWindow
	var dirEdit *walk.LineEdit
	var tableView *walk.TableView
	var statusLabel *walk.Label
	var scanStatsLabel *walk.Label
	var progressBar *walk.ProgressBar
	var deleteBtn *walk.PushButton
	var scanBtn *walk.PushButton
	var cancelBtn *walk.PushButton
	
	// Context and cancel function for scan operations
	var cancelScan context.CancelFunc
	var ctx context.Context
	
	// Create live stats
	liveStats := &LiveStats{
		ExtensionStats: make(map[string]*ExtStats),
	}
	
	// Create the table model
	model := new(FileTypeTableModel)
	
	// List of unwanted files found during scan
	var unwantedFiles []FileInfo

	MainWindow{
		AssignTo: &mainWindow,
		Title:    "Plex Cleanup Tool",
		MinSize:  Size{600, 600},
		Layout:   VBox{},
		OnKeyDown: func(key walk.Key) {
			if key == walk.KeyEscape {
				if cancelScan != nil {
					cancelScan()
				}
			}
		},
		Children: []Widget{
			GroupBox{
				Title:  "Directory Selection",
				Layout: HBox{},
				Children: []Widget{
					LineEdit{
						AssignTo: &dirEdit,
						ReadOnly: true,
					},
					PushButton{
						AssignTo: &scanBtn,
						Text: "Browse & Scan...",
						OnClicked: func() {
							dlg := new(walk.FileDialog)
							dlg.Title = "Select Plex Library Directory"
							dlg.FilePath = dirEdit.Text()
							
							if ok, _ := dlg.ShowBrowseFolder(mainWindow); !ok {
								return
							}
							
							dirEdit.SetText(dlg.FilePath)
							
							// Reset stats and UI
							liveStats = &LiveStats{
								ExtensionStats: make(map[string]*ExtStats),
							}
							unwantedFiles = nil
							
							// Update UI
							model.items = nil
							tableView.SetModel(model)
							
							// Update button states
							deleteBtn.SetEnabled(false)
							scanBtn.SetEnabled(false)
							cancelBtn.SetEnabled(true)
							statusLabel.SetText("Scanning directory...")
							scanStatsLabel.SetText("Files scanned: 0 | Directories scanned: 0")
							
							// Create cancellable context
							ctx, cancelScan = context.WithCancel(context.Background())
							
							// Update progress bar
							progressBar.SetValue(0)
							progressBar.SetRange(0, 100)
							
							// Run scan in goroutine
							go func() {
								// Create update UI function
								updateUI := func() {
									mainWindow.Synchronize(func() {
										// Lock while accessing stats
										liveStats.mutex.Lock()
										defer liveStats.mutex.Unlock()
										
										// Update live stats label
										scanStatsLabel.SetText(fmt.Sprintf("Files scanned: %d | Directories scanned: %d | Unwanted files found: %d (%s)",
											liveStats.FilesScanned, liveStats.DirsScanned, liveStats.TotalFiles, formatSize(liveStats.TotalSize)))
										
										// Update model with current scan results
										model.items = make([]FileTypeItem, 0, len(liveStats.ExtensionStats)+1)
										
										// Add each extension type
										for ext, stat := range liveStats.ExtensionStats {
											displayExt := ext
											if ext == ".numbered" {
												displayExt = "Numbered files (.001, .002, etc.)"
											} else if ext == ".rXX" {
												displayExt = "RAR parts (S01E01 -.r08, -.r09, etc.)"
											}
											
											model.items = append(model.items, FileTypeItem{
												FileType:  displayExt,
												Count:     stat.Count,
												TotalSize: formatSize(stat.Size),
											})
										}
										
										// Add total row if we have files
										if liveStats.TotalFiles > 0 {
											model.items = append(model.items, FileTypeItem{
												FileType:  "TOTAL",
												Count:     liveStats.TotalFiles,
												TotalSize: formatSize(liveStats.TotalSize),
												IsTotal:   true,
											})
										}
										
										// Refresh table
										tableView.SetModel(model)
									})
								}
								
								// Collect unwanted files
								var filesToDelete []FileInfo
								
								// Run scan
								err := filepath.Walk(dlg.FilePath, func(path string, info os.FileInfo, err error) error {
									// Check for cancellation
									select {
									case <-ctx.Done():
										return ctx.Err()
									default:
										// Continue processing
									}
									
									if err != nil {
										return err
									}
									
									// Update directory count
									if info.IsDir() {
										liveStats.mutex.Lock()
										liveStats.DirsScanned++
										liveStats.mutex.Unlock()
										updateUI()
										return nil
									}
									
									// Update file count
									liveStats.mutex.Lock()
									liveStats.FilesScanned++
									
									// Check if file is unwanted
									if isUnwantedFile(info.Name()) {
										fileExt := getFileExtension(info.Name())
										
										if _, exists := liveStats.ExtensionStats[fileExt]; !exists {
											liveStats.ExtensionStats[fileExt] = &ExtStats{}
										}
										
										liveStats.ExtensionStats[fileExt].Count++
										liveStats.ExtensionStats[fileExt].Size += info.Size()
										liveStats.TotalFiles++
										liveStats.TotalSize += info.Size()
										
										// Add to deletion list
										filesToDelete = append(filesToDelete, FileInfo{
											Path: path,
											Size: info.Size(),
										})
									}
									liveStats.mutex.Unlock()
									
									// Update UI periodically
									if liveStats.FilesScanned%100 == 0 {
										updateUI()
									}
									
									return nil
								})
								
								// Final update
								updateUI()
								
								// Store unwanted files for deletion
								unwantedFiles = filesToDelete
								
								// Update UI in UI thread with final results
								mainWindow.Synchronize(func() {
									if err != nil && err != context.Canceled {
										walk.MsgBox(mainWindow, "Error", "Failed to scan directory: "+err.Error(), walk.MsgBoxIconError)
										statusLabel.SetText("Scan failed")
									} else if err == context.Canceled {
										statusLabel.SetText("Scan cancelled")
									} else {
										if liveStats.TotalFiles == 0 {
											statusLabel.SetText("No unwanted files found")
										} else {
											statusLabel.SetText(fmt.Sprintf("Found %d unwanted files (%s)",
												liveStats.TotalFiles, formatSize(liveStats.TotalSize)))
											deleteBtn.SetEnabled(true)
										}
									}
									
									// Enable scan button, disable cancel
									scanBtn.SetEnabled(true)
									cancelBtn.SetEnabled(false)
								})
							}()
						},
					},
					PushButton{
						AssignTo: &cancelBtn,
						Text:     "Cancel",
						Enabled:  false,
						OnClicked: func() {
							if cancelScan != nil {
								cancelScan()
								statusLabel.SetText("Cancelling scan...")
							}
						},
					},
				},
			},
			Label{
				AssignTo: &scanStatsLabel,
				Text:     "Files scanned: 0 | Directories scanned: 0",
			},
			TableView{
				AssignTo:      &tableView,
				StretchFactor: 2,
				Columns: []TableViewColumn{
					{Title: "File Type", Width: 250},
					{Title: "Count", Width: 100},
					{Title: "Total Size", Width: 150},
				},
				StyleCell: func(style *walk.CellStyle) {
					if len(model.items) <= style.Row() {
						return
					}
					
					item := model.items[style.Row()]
					
					if item.IsTotal {
						style.TextColor = walk.RGB(0, 0, 128)
						if font, err := walk.NewFont("Segoe UI", 9, walk.FontBold); err == nil {
							style.Font = font
						}
					}
				},
			},
			Label{
				AssignTo: &statusLabel,
				Text:     "Ready to scan",
			},
			ProgressBar{
				AssignTo: &progressBar,
				MaxValue: 100,
				MinValue: 0,
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					PushButton{
						AssignTo:  &deleteBtn,
						Text:      "Delete Unwanted Files",
						Enabled:   false,
						OnClicked: func() {
							if len(unwantedFiles) == 0 {
								return
							}
							
							// Confirm deletion
							if walk.MsgBox(mainWindow, "Confirm Deletion", 
								fmt.Sprintf("Are you sure you want to delete %d files (%s)?", 
									liveStats.TotalFiles, formatSize(liveStats.TotalSize)), 
									walk.MsgBoxOKCancel|walk.MsgBoxIconQuestion) != walk.DlgCmdOK {
								return
							}
							
							// Disable buttons during deletion
							deleteBtn.SetEnabled(false)
							scanBtn.SetEnabled(false)
							cancelBtn.SetEnabled(true)
							statusLabel.SetText("Deleting files...")
							
							// Create cancellable context for deletion
							ctx, cancelScan = context.WithCancel(context.Background())
							
							// Delete files in goroutine
							go func() {
								startTime := time.Now()
								deletedCount, deletedSize := deleteFiles(ctx, unwantedFiles, progressBar)
								duration := time.Since(startTime)
								
								// Log results
								log.Printf("Cleanup completed in %.2f seconds\n", duration.Seconds())
								log.Printf("Total files deleted: %d\n", deletedCount)
								log.Printf("Total space freed: %s\n", formatSize(deletedSize))
								
								// Update UI
								mainWindow.Synchronize(func() {
									cancelBtn.SetEnabled(false)
									scanBtn.SetEnabled(true)
									
									if ctx.Err() == context.Canceled {
										statusLabel.SetText(fmt.Sprintf("Deletion cancelled. Deleted %d files (%s)", 
											deletedCount, formatSize(deletedSize)))
									} else {
										statusLabel.SetText(fmt.Sprintf("Deleted %d files (%s)", 
											deletedCount, formatSize(deletedSize)))
										
										// Show completion dialog
										walk.MsgBox(mainWindow, "Cleanup Complete", 
											fmt.Sprintf("Deleted %d files\nFreed %s of disk space\nLog file: %s", 
												deletedCount, formatSize(deletedSize), logFile), 
												walk.MsgBoxIconInformation)
									}
									
									// Clear data
									liveStats = &LiveStats{
										ExtensionStats: make(map[string]*ExtStats),
									}
									unwantedFiles = nil
									model.items = nil
									tableView.SetModel(model)
									
									scanStatsLabel.SetText("Files scanned: 0 | Directories scanned: 0")
								})
							}()
						},
					},
					HSpacer{},
					PushButton{
						Text:       "Cancel",
						Enabled:    false,
						AssignTo:   &cancelBtn,
						OnClicked: func() {
							if cancelScan != nil {
								cancelScan()
							}
						},
					},
				},
			},
		},
	}.Run()
}
