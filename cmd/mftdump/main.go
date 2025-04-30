package main

import (
        "encoding/csv"
        "flag"
        "fmt"
        "io"
        "os"
        "path/filepath"
        "runtime"
        "strings"
        "time"

        "github.com/lideming/gomft/bootsect"
        "github.com/lideming/gomft/fragment"
        "github.com/lideming/gomft/mft"
)

const supportedOemId = "NTFS    "

const (
        exitCodeUserError int = iota + 2
        exitCodeFunctionalError
        exitCodeTechnicalError
)

const isWin = runtime.GOOS == "windows"

var (
        // flags
        verbose                 = false
        overwriteOutputIfExists = false
        showProgress            = false
        csvOutput               = false
        csvOutputOnly           = false
        csvOutputFile           = ""
        fullPathOutput          = false
)

// PathNode represents a node in the file path tree
type PathNode struct {
        RecordNumber uint64
        Name         string
        Parent       uint64
        IsDirectory  bool
}

// RecordInfo contains information about an MFT record
type RecordInfo struct {
        RecordNumber       uint64
        Parent             uint64
        IsDirectory        bool
        IsInUse            bool
        Name               string
        FileNameAttributes []mft.FileName
}

// GetRecordInfo extracts useful information from an MFT record
func GetRecordInfo(record mft.Record) RecordInfo {
        info := RecordInfo{
                RecordNumber: record.FileReference.RecordNumber,
                IsInUse:      record.IsInUse(),
                IsDirectory:  record.IsDirectory(),
        }
        
        // Get file name attributes
        fileNameAttrs := record.FindAttributes(mft.AttributeTypeFileName)
        for _, attr := range fileNameAttrs {
                if !attr.Resident {
                        continue
                }
                
                fileNameAttr, err := mft.ParseFileName(attr)
                if err != nil {
                        continue
                }
                
                // Skip system names
                if fileNameAttr.Namespace != mft.FileNameNamespaceWin32 && 
                   fileNameAttr.Namespace != mft.FileNameNamespaceWin32Dos && 
                   fileNameAttr.Namespace != mft.FileNameNamespacePosix {
                        continue
                }
                
                // Prefer Win32 namespace if available
                if fileNameAttr.Namespace == mft.FileNameNamespaceWin32 || 
                   fileNameAttr.Namespace == mft.FileNameNamespaceWin32Dos || 
                   info.Name == "" {
                        info.Name = fileNameAttr.Name
                        info.Parent = fileNameAttr.ParentFileReference.RecordNumber
                }
                
                info.FileNameAttributes = append(info.FileNameAttributes, fileNameAttr)
        }
        
        return info
}

// buildFullPath builds the full path for a record
func buildFullPath(recordNumber uint64, pathMap map[uint64]PathNode) string {
        var path []string
        current, ok := pathMap[recordNumber]
        
        // If record not found in path map, return empty string
        if !ok {
                return ""
        }
        
        // Add current node name to path
        path = append(path, current.Name)
        
        // Traverse up the tree
        for {
                parent, ok := pathMap[current.Parent]
                if !ok {
                        break
                }
                
                // Avoid cycles
                if parent.RecordNumber == current.RecordNumber {
                        break
                }
                
                // Add parent name to path
                path = append([]string{parent.Name}, path...)
                
                // Move up to parent
                current = parent
        }
        
        // Join path components
        return strings.Join(path, "\\")
}

func main() {
        start := time.Now()
        verboseFlag := flag.Bool("v", false, "verbose; print details about what's going on")
        forceFlag := flag.Bool("f", false, "force; overwrite the output file if it already exists")
        progressFlag := flag.Bool("p", false, "progress; show progress during dumping")
        csvFlag := flag.Bool("csv", false, "output CSV data")
        csvOnlyFlag := flag.Bool("csv-only", false, "output only CSV data (no MFT dump)")
        csvFileFlag := flag.String("csv-file", "", "output CSV data to file (implies -csv)")
        fullPathFlag := flag.Bool("full-path", false, "include full path in CSV output")

        flag.Usage = printUsage
        flag.Parse()

        verbose = *verboseFlag
        overwriteOutputIfExists = *forceFlag
        showProgress = *progressFlag
        csvOutput = *csvFlag
        csvOutputOnly = *csvOnlyFlag
        csvOutputFile = *csvFileFlag
        fullPathOutput = *fullPathFlag

        if *csvFileFlag != "" {
                csvOutput = true
        }

        args := flag.Args()

        if len(args) != 2 && !csvOutputOnly {
                printUsage()
                os.Exit(exitCodeUserError)
                return
        }

        if len(args) != 1 && csvOutputOnly {
                printUsage()
                os.Exit(exitCodeUserError)
                return
        }

        volume := args[0]
        if isWin {
                volume = `\\.\` + volume
        }

        var outfile string
        if !csvOutputOnly {
                outfile = args[1]
        }

        // Generate CSV file name if not specified
        var csvFilePath string
        if csvOutput {
                if csvOutputFile != "" {
                        csvFilePath = csvOutputFile
                } else {
                        // Generate CSV file name based on input file
                        volumeName := filepath.Base(volume)
                        if volumeName == "" || volumeName == "." || volumeName == ".." {
                                volumeName = "volume"
                        }
                        csvFilePath = fmt.Sprintf("%s_mft.csv", volumeName)
                }
        }

        in, err := os.Open(volume)
        if err != nil {
                fatalf(exitCodeTechnicalError, "Unable to open volume using path %s: %v\n", volume, err)
        }
        defer in.Close()

        printVerbose("Reading boot sector\n")
        bootSectorData := make([]byte, 512)
        _, err = io.ReadFull(in, bootSectorData)
        if err != nil {
                fatalf(exitCodeTechnicalError, "Unable to read boot sector: %v\n", err)
        }

        printVerbose("Read %d bytes of boot sector, parsing boot sector\n", len(bootSectorData))
        bootSector, err := bootsect.Parse(bootSectorData)
        if err != nil {
                fatalf(exitCodeTechnicalError, "Unable to parse boot sector data: %v\n", err)
        }

        if bootSector.OemId != supportedOemId {
                fatalf(exitCodeFunctionalError, "Unknown OemId (file system type) %q (expected %q)\n", bootSector.OemId, supportedOemId)
        }

        bytesPerCluster := bootSector.BytesPerSector * bootSector.SectorsPerCluster
        mftPosInBytes := int64(bootSector.MftClusterNumber) * int64(bytesPerCluster)

        _, err = in.Seek(mftPosInBytes, 0)
        if err != nil {
                fatalf(exitCodeTechnicalError, "Unable to seek to MFT position: %v\n", err)
        }

        mftSizeInBytes := bootSector.FileRecordSegmentSizeInBytes
        printVerbose("Reading $MFT file record at position %d (size: %d bytes)\n", mftPosInBytes, mftSizeInBytes)
        mftData := make([]byte, mftSizeInBytes)
        _, err = io.ReadFull(in, mftData)
        if err != nil {
                fatalf(exitCodeTechnicalError, "Unable to read $MFT record: %v\n", err)
        }

        printVerbose("Parsing $MFT file record\n")
        record, err := mft.ParseRecord(mftData)
        if err != nil {
                fatalf(exitCodeTechnicalError, "Unable to parse $MFT record: %v\n", err)
        }

        printVerbose("Reading $DATA attribute in $MFT file record\n")
        dataAttributes := record.FindAttributes(mft.AttributeTypeData)
        if len(dataAttributes) == 0 {
                fatalf(exitCodeTechnicalError, "No $DATA attribute found in $MFT record\n")
        }

        if len(dataAttributes) > 1 {
                fatalf(exitCodeTechnicalError, "More than 1 $DATA attribute found in $MFT record\n")
        }

        dataAttribute := dataAttributes[0]
        if dataAttribute.Resident {
                fatalf(exitCodeTechnicalError, "Don't know how to handle resident $DATA attribute in $MFT record\n")
        }

        dataRuns, err := mft.ParseDataRuns(dataAttribute.Data)
        if err != nil {
                fatalf(exitCodeTechnicalError, "Unable to parse dataruns in $MFT $DATA record: %v\n", err)
        }

        if len(dataRuns) == 0 {
                fatalf(exitCodeTechnicalError, "No dataruns found in $MFT $DATA record\n")
        }

        fragments := mft.DataRunsToFragments(dataRuns, bytesPerCluster)
        totalLength := int64(0)
        for _, frag := range fragments {
                totalLength += int64(frag.Length)
        }

        // Extract MFT to temporary file for CSV generation
        if csvOutput {
                tempMftFile := filepath.Join(os.TempDir(), "mft.bin")
                tempOut, err := os.Create(tempMftFile)
                if err != nil {
                        fatalf(exitCodeTechnicalError, "Unable to create temporary MFT file: %v\n", err)
                }
                
                printVerbose("Copying MFT to temporary file for CSV generation\n")
                _, err = copy(tempOut, fragment.NewReader(in, fragments), totalLength)
                if err != nil {
                        tempOut.Close()
                        os.Remove(tempMftFile)
                        fatalf(exitCodeTechnicalError, "Error copying MFT to temporary file: %v\n", err)
                }
                tempOut.Close()
                
                // Generate CSV
                printVerbose("Generating CSV file %s\n", csvFilePath)
                err = generateCSV(tempMftFile, csvFilePath)
                if err != nil {
                        os.Remove(tempMftFile)
                        fatalf(exitCodeTechnicalError, "Error generating CSV: %v\n", err)
                }
                
                os.Remove(tempMftFile)
                printVerbose("CSV file generated successfully\n")
                
                // If CSV only, exit
                if csvOutputOnly {
                        end := time.Now()
                        dur := end.Sub(start)
                        printVerbose("Finished in %v\n", dur)
                        return
                }
                
                // Reset file position for MFT dump
                _, err = in.Seek(mftPosInBytes, 0)
                if err != nil {
                        fatalf(exitCodeTechnicalError, "Unable to seek to MFT position: %v\n", err)
                }
        }

        // Continue with normal MFT dump
        out, err := openOutputFile(outfile)
        if err != nil {
                fatalf(exitCodeFunctionalError, "Unable to open output file: %v\n", err)
        }
        defer out.Close()

        printVerbose("Copying %d bytes (%s) of data to %s\n", totalLength, formatBytes(totalLength), outfile)
        n, err := copy(out, fragment.NewReader(in, fragments), totalLength)
        if err != nil {
                fatalf(exitCodeTechnicalError, "Error copying data to output file: %v\n", err)
        }

        if n != totalLength {
                fatalf(exitCodeTechnicalError, "Expected to copy %d bytes, but copied only %d\n", totalLength, n)
        }
        end := time.Now()
        dur := end.Sub(start)
        printVerbose("Finished in %v\n", dur)
}

func generateCSV(mftFilePath, csvFilePath string) error {
        mftFile, err := os.Open(mftFilePath)
        if err != nil {
                return fmt.Errorf("error opening MFT file: %v", err)
        }
        defer mftFile.Close()

        csvFile, err := os.Create(csvFilePath)
        if err != nil {
                return fmt.Errorf("error creating CSV file: %v", err)
        }
        defer csvFile.Close()

        // Create a CSV writer
        writer := csv.NewWriter(csvFile)
        defer writer.Flush()

        // Write CSV header
        header := []string{
                "RecordNumber", "FileName", "ParentRecordNumber", "IsDirectory", 
                "IsDeleted", "SizeAllocated", "SizeActual", "Created", "Modified", 
                "MftModified", "Accessed",
        }
        
        if fullPathOutput {
                header = append([]string{"FullPath"}, header...)
        }
        
        if err := writer.Write(header); err != nil {
                return fmt.Errorf("error writing CSV header: %v", err)
        }

        // Get file size
        fileInfo, err := mftFile.Stat()
        if err != nil {
                return fmt.Errorf("error getting file info: %v", err)
        }
        fileSize := fileInfo.Size()
        
        // First pass: build path map if full path output is requested
        pathMap := make(map[uint64]PathNode)
        
        if fullPathOutput {
                if verbose {
                        fmt.Println("Building path map for full path resolution...")
                }
                
                // Reset file position
                if _, err := mftFile.Seek(0, 0); err != nil {
                        return fmt.Errorf("error seeking to beginning of file: %v", err)
                }
                
                // Read records
                for offset := int64(0); offset < fileSize; offset += 1024 {
                        // Read record data
                        recordData := make([]byte, 1024)
                        n, err := mftFile.Read(recordData)
                        if err != nil {
                                if err == io.EOF {
                                        break
                                }
                                return fmt.Errorf("error reading MFT record: %v", err)
                        }
                        
                        if n < 1024 {
                                break
                        }
                        
                        // Parse record
                        record, err := mft.ParseRecord(recordData)
                        if err != nil {
                                continue
                        }
                        
                        // Get record info
                        info := GetRecordInfo(record)
                        
                        if !info.IsInUse || info.Name == "" {
                                continue
                        }
                        
                        // Add to path map
                        pathMap[info.RecordNumber] = PathNode{
                                RecordNumber: info.RecordNumber,
                                Name:         info.Name,
                                Parent:       info.Parent,
                                IsDirectory:  info.IsDirectory,
                        }
                }
                
                // Reset file position for second pass
                if _, err := mftFile.Seek(0, 0); err != nil {
                        return fmt.Errorf("error seeking to beginning of file: %v", err)
                }
        }

        // Second pass: generate CSV records
        if verbose {
                fmt.Println("Generating CSV records...")
        }
        
        for offset := int64(0); offset < fileSize; offset += 1024 {
                // Read record data
                recordData := make([]byte, 1024)
                n, err := mftFile.Read(recordData)
                if err != nil {
                        if err == io.EOF {
                                break
                        }
                        return fmt.Errorf("error reading MFT record: %v", err)
                }
                
                if n < 1024 {
                        break
                }
                
                // Parse record
                record, err := mft.ParseRecord(recordData)
                if err != nil {
                        continue
                }
                
                // Get record info
                info := GetRecordInfo(record)
                
                if !info.IsInUse || info.Name == "" || len(info.FileNameAttributes) == 0 {
                        continue
                }
                
                // Process each file name attribute
                for _, fileNameAttr := range info.FileNameAttributes {
                        // Create CSV record - the CSV writer will handle quoting automatically
                        csvRecord := []string{
                                fmt.Sprintf("%d", info.RecordNumber),
                                info.Name,
                                fmt.Sprintf("%d", info.Parent),
                                fmt.Sprintf("%t", info.IsDirectory),
                                fmt.Sprintf("%t", !info.IsInUse),
                                fmt.Sprintf("%d", fileNameAttr.AllocatedSize),
                                fmt.Sprintf("%d", fileNameAttr.ActualSize),
                                fileNameAttr.Creation.Format(time.RFC3339),
                                fileNameAttr.FileLastModified.Format(time.RFC3339),
                                fileNameAttr.MftLastModified.Format(time.RFC3339),
                                fileNameAttr.LastAccess.Format(time.RFC3339),
                        }
                        
                        // Add full path if requested
                        if fullPathOutput {
                                fullPath := buildFullPath(info.RecordNumber, pathMap)
                                csvRecord = append([]string{fullPath}, csvRecord...)
                        }
                        
                        if err := writer.Write(csvRecord); err != nil {
                                return fmt.Errorf("error writing CSV record: %v", err)
                        }
                }
        }
        
        if verbose {
                fmt.Printf("CSV data written to %s\n", csvFilePath)
        }
        
        return nil
}

func copy(dst io.Writer, src io.Reader, totalLength int64) (written int64, err error) {
        buf := make([]byte, 1024*1024)
        if !showProgress {
                return io.CopyBuffer(dst, src, buf)
        }

        onePercent := float64(totalLength) / float64(100.0)
        totalSize := formatBytes(totalLength)

        // Below copied from io.copyBuffer (https://golang.org/src/io/io.go?s=12796:12856#L380)
        for {
                printProgress(written, totalSize, onePercent)

                nr, er := src.Read(buf)
                if nr > 0 {
                        nw, ew := dst.Write(buf[0:nr])
                        if nw > 0 {
                                written += int64(nw)
                        }
                        if ew != nil {
                                err = ew
                                break
                        }
                        if nr != nw {
                                err = io.ErrShortWrite
                                break
                        }
                }
                if er != nil {
                        if er != io.EOF {
                                err = er
                        }
                        break
                }
        }
        printProgress(written, totalSize, onePercent)
        fmt.Println()
        return written, err
}

func printProgress(n int64, totalSize string, onePercent float64) {
        percentage := float64(n) / onePercent
        barCount := int(percentage / 2.0)
        spaceCount := 50 - barCount
        fmt.Printf("\r[%s%s] %.2f%% (%s / %s)     ", strings.Repeat("|", barCount), strings.Repeat(" ", spaceCount), percentage, formatBytes(n), totalSize)
}

func openOutputFile(outfile string) (*os.File, error) {
        if overwriteOutputIfExists {
                return os.Create(outfile)
        } else {
                return os.OpenFile(outfile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
        }
}

func printUsage() {
        out := os.Stderr
        exe := filepath.Base(os.Args[0])
        fmt.Fprintf(out, "\nusage: %s [flags] <volume> <output file>\n", exe)
        fmt.Fprintf(out, "       %s -csv-only [flags] <volume>\n\n", exe)
        fmt.Fprintln(out, "Dump the MFT of a volume to a file. The volume should be NTFS formatted.")
        fmt.Fprintln(out, "Optionally generate a CSV file with MFT information.")
        fmt.Fprintln(out, "\nFlags:")

        flag.PrintDefaults()

        fmt.Fprintf(out, "\nExamples:\n")
        if isWin {
                fmt.Fprintf(out, "  %s -v -f C: D:\\c.mft\n", exe)
                fmt.Fprintf(out, "  %s -csv C: D:\\c.mft\n", exe)
                fmt.Fprintf(out, "  %s -csv-file D:\\c_mft.csv C: D:\\c.mft\n", exe)
                fmt.Fprintf(out, "  %s -csv-only C:\n", exe)
                fmt.Fprintf(out, "  %s -csv-only -full-path C:\n", exe)
        } else {
                fmt.Fprintf(out, "  %s -v -f /dev/sdb1 ~/sdb1.mft\n", exe)
                fmt.Fprintf(out, "  %s -csv /dev/sdb1 ~/sdb1.mft\n", exe)
                fmt.Fprintf(out, "  %s -csv-file ~/sdb1_mft.csv /dev/sdb1 ~/sdb1.mft\n", exe)
                fmt.Fprintf(out, "  %s -csv-only /dev/sdb1\n", exe)
                fmt.Fprintf(out, "  %s -csv-only -full-path /dev/sdb1\n", exe)
        }
}

func fatalf(exitCode int, format string, v ...interface{}) {
        fmt.Printf(format, v...)
        os.Exit(exitCode)
}

func printVerbose(format string, v ...interface{}) {
        if verbose {
                fmt.Printf(format, v...)
        }
}

func formatBytes(b int64) string {
        if b < 1024 {
                return fmt.Sprintf("%dB", b)
        }
        if b < 1048576 {
                return fmt.Sprintf("%.2fKiB", float32(b)/float32(1024))
        }
        if b < 1073741824 {
                return fmt.Sprintf("%.2fMiB", float32(b)/float32(1048576))
        }
        return fmt.Sprintf("%.2fGiB", float32(b)/float32(1073741824))
}