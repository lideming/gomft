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

        "github.com/t9t/gomft/bootsect"
        "github.com/t9t/gomft/fragment"
        "github.com/t9t/gomft/mft"
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
)

func main() {
        start := time.Now()
        verboseFlag := flag.Bool("v", false, "verbose; print details about what's going on")
        forceFlag := flag.Bool("f", false, "force; overwrite the output file if it already exists")
        progressFlag := flag.Bool("p", false, "progress; show progress during dumping")
        csvFlag := flag.Bool("csv", false, "output MFT records as CSV file")
        csvOnlyFlag := flag.Bool("csv-only", false, "output only CSV file without dumping the MFT")
        csvFileFlag := flag.String("csv-file", "", "CSV output file path (if not specified, uses <output file>.csv)")

        flag.Usage = printUsage
        flag.Parse()

        verbose = *verboseFlag
        overwriteOutputIfExists = *forceFlag
        showProgress = *progressFlag
        csvOutput = *csvFlag || *csvOnlyFlag
        csvOutputOnly = *csvOnlyFlag
        csvOutputFile = *csvFileFlag
        args := flag.Args()

        if len(args) != 2 {
                printUsage()
                os.Exit(exitCodeUserError)
                return
        }

        volume := args[0]
        if isWin {
                volume = `\\.\` + volume
        }
        outfile := args[1]

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

        // Skip MFT dump if CSV-only mode is enabled
        if !csvOutputOnly {
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
        } else {
            printVerbose("CSV-only mode enabled, skipping MFT dump\n")
        }
        
        // Process CSV output if requested
        if csvOutput {
            if csvOutputFile == "" {
                if csvOutputOnly {
                    // If CSV-only mode and no CSV file specified, use the output file name
                    csvOutputFile = outfile
                } else {
                    // Otherwise append .csv to the output file name
                    csvOutputFile = outfile + ".csv"
                }
            }
            printVerbose("Generating CSV output to %s\n", csvOutputFile)
            
            // In CSV-only mode, we need to read directly from the volume
            // In normal mode, we read from the dumped MFT file
            if csvOutputOnly {
                err = generateCSVFromVolume(volume, csvOutputFile, fragments, bytesPerCluster)
            } else {
                err = generateCSV(volume, outfile, csvOutputFile)
            }
            
            if err != nil {
                fatalf(exitCodeTechnicalError, "Error generating CSV output: %v\n", err)
            }
        }
        
        end := time.Now()
        dur := end.Sub(start)
        printVerbose("Finished in %v\n", dur)
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
        fmt.Fprintf(out, "\nusage: %s [flags] <volume> <output file>\n\n", exe)
        fmt.Fprintln(out, "Dump the MFT of a volume to a file. The volume should be NTFS formatted.")
        fmt.Fprintln(out, "\nFlags:")

        flag.PrintDefaults()

        fmt.Fprintf(out, "\nFor example: ")
        if isWin {
                fmt.Fprintf(out, "%s -v -f C: D:\\c.mft\n", exe)
                fmt.Fprintf(out, "%s -v -f -csv C: D:\\c.mft\n", exe)
        fmt.Fprintf(out, "%s -v -f -csv-only C: D:\\c.csv\n", exe)
        } else {
                fmt.Fprintf(out, "%s -v -f /dev/sdb1 ~/sdb1.mft\n", exe)
                fmt.Fprintf(out, "%s -v -f -csv /dev/sdb1 ~/sdb1.mft\n", exe)
                fmt.Fprintf(out, "%s -v -f -csv-only /dev/sdb1 ~/sdb1.csv\n", exe)
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

// generateCSV reads the MFT file and generates a CSV file with MFT record information
// generateCSVFromVolume generates a CSV file directly from the volume without creating an intermediate MFT dump
func generateCSVFromVolume(volumePath, csvPath string, fragments []fragment.Fragment, bytesPerCluster int) error {
    // Open the volume
    volume, err := os.Open(volumePath)
    if err != nil {
        return fmt.Errorf("error opening volume: %v", err)
    }
    defer volume.Close()
    
    // Create CSV file
    csvFile, err := os.Create(csvPath)
    if err != nil {
        return fmt.Errorf("error creating CSV file: %v", err)
    }
    defer csvFile.Close()
    
    // Create CSV writer
    writer := csv.NewWriter(csvFile)
    defer writer.Flush()
    
    // Write CSV header
    header := []string{
        "RecordNumber",
        "SequenceNumber",
        "InUse",
        "IsDirectory",
        "HasFileName",
        "FileName",
        "ParentRecordNumber",
        "ParentSequenceNumber",
        "Creation",
        "LastModified",
        "MftLastModified",
        "LastAccess",
        "AllocatedSize",
        "ActualSize",
        "FileAttributes",
    }
    if err := writer.Write(header); err != nil {
        return fmt.Errorf("error writing CSV header: %v", err)
    }
    
    // Create a fragment reader to read the MFT
    fragReader := fragment.NewReader(volume, fragments)
    
    // Use default record size of 1024 bytes
    recordSize := 1024
    
    // Process each record
    recordBuf := make([]byte, recordSize)
    recordCount := 0
    
    for {
        _, err := io.ReadFull(fragReader, recordBuf)
        if err != nil {
            if err == io.EOF {
                break
            }
            return fmt.Errorf("error reading MFT record: %v", err)
        }
        
        record, err := mft.ParseRecord(recordBuf)
        if err != nil {
            // Skip invalid records
            continue
        }
        
        recordCount++
        if recordCount % 1000 == 0 && verbose {
            fmt.Printf("Processed %d records\n", recordCount)
        }
        
        // Extract file name if available
        hasFileName := false
        fileName := ""
        parentRecordNumber := uint64(0)
        parentSequenceNumber := uint16(0)
        creation := time.Time{}
        lastModified := time.Time{}
        mftLastModified := time.Time{}
        lastAccess := time.Time{}
        allocatedSize := int64(0)
        actualSize := int64(0)
        fileAttributes := uint32(0)
        
        // Look for file name attribute
        fileNameAttrs := record.FindAttributes(mft.AttributeTypeFileName)
        if len(fileNameAttrs) > 0 {
            hasFileName = true
            for _, attr := range fileNameAttrs {
                if attr.Resident {
                    fileNameAttr, err := mft.ParseFileName(attr.Data)
                    if err == nil {
                        // Prefer Win32 namespace (0x03) if available
                        if fileNameAttr.Namespace == mft.FileNameNamespaceWin32Dos || fileName == "" {
                            fileName = fileNameAttr.Name
                            parentRecordNumber = fileNameAttr.ParentFileReference.RecordNumber
                            parentSequenceNumber = fileNameAttr.ParentFileReference.SequenceNumber
                            creation = fileNameAttr.Creation
                            lastModified = fileNameAttr.FileLastModified
                            mftLastModified = fileNameAttr.MftLastModified
                            lastAccess = fileNameAttr.LastAccess
                            allocatedSize = int64(fileNameAttr.AllocatedSize)
                            actualSize = int64(fileNameAttr.ActualSize)
                            fileAttributes = uint32(fileNameAttr.Flags)
                        }
                    }
                }
            }
        }
        
        // Write record to CSV
        row := []string{
            fmt.Sprintf("%d", record.FileReference.RecordNumber),
            fmt.Sprintf("%d", record.FileReference.SequenceNumber),
            fmt.Sprintf("%t", record.Flags.Is(mft.RecordFlagInUse)),
            fmt.Sprintf("%t", record.Flags.Is(mft.RecordFlagIsDirectory)),
            fmt.Sprintf("%t", hasFileName),
            fileName,
            fmt.Sprintf("%d", parentRecordNumber),
            fmt.Sprintf("%d", parentSequenceNumber),
            creation.Format(time.RFC3339),
            lastModified.Format(time.RFC3339),
            mftLastModified.Format(time.RFC3339),
            lastAccess.Format(time.RFC3339),
            fmt.Sprintf("%d", allocatedSize),
            fmt.Sprintf("%d", actualSize),
            fmt.Sprintf("%d", fileAttributes),
        }
        if err := writer.Write(row); err != nil {
            return fmt.Errorf("error writing CSV row: %v", err)
        }
    }
    
    if verbose {
        fmt.Printf("Processed %d records total\n", recordCount)
    }
    
    return nil
}

// filetimeToTime converts a Windows filetime to a Go time.Time
func filetimeToTime(filetime uint64) time.Time {
    // Windows filetime is 100-nanosecond intervals since January 1, 1601 UTC
    const hundredNanosecondIntervals = 10000000
    const secondsBetween1601And1970 = 11644473600
    
    if filetime == 0 {
        return time.Time{}
    }
    
    // Convert to seconds and adjust for the difference between Windows and Unix epoch
    seconds := int64(filetime / hundredNanosecondIntervals)
    nanoseconds := int64((filetime % hundredNanosecondIntervals) * 100)
    
    // Adjust for the difference between Windows epoch (1601) and Unix epoch (1970)
    seconds -= secondsBetween1601And1970
    
    return time.Unix(seconds, nanoseconds).UTC()
}

func generateCSV(volume, mftFile, csvFile string) error {
        printVerbose("Opening MFT file %s for CSV processing\n", mftFile)
        mftIn, err := os.Open(mftFile)
        if err != nil {
                return fmt.Errorf("unable to open MFT file: %v", err)
        }
        defer mftIn.Close()

        printVerbose("Creating CSV file %s\n", csvFile)
        csvOut, err := os.Create(csvFile)
        if err != nil {
                return fmt.Errorf("unable to create CSV file: %v", err)
        }
        defer csvOut.Close()

        // Create CSV writer
        writer := csv.NewWriter(csvOut)
        defer writer.Flush()

        // Write CSV header
        header := []string{
                "RecordNumber",
                "SequenceNumber",
                "InUse",
                "IsDirectory",
                "HasFileName",
                "FileName",
                "ParentRecordNumber",
                "ParentSequenceNumber",
                "Creation",
                "LastModified",
                "MftLastModified",
                "LastAccess",
                "AllocatedSize",
                "ActualSize",
                "FileAttributes",
        }
        if err := writer.Write(header); err != nil {
                return fmt.Errorf("error writing CSV header: %v", err)
        }

        // Read MFT records
        recordSize := uint32(1024) // Default MFT record size, could be different
        buffer := make([]byte, recordSize)
        recordNumber := uint64(0)

        printVerbose("Processing MFT records...\n")
        for {
                _, err := io.ReadFull(mftIn, buffer)
                if err == io.EOF || err == io.ErrUnexpectedEOF {
                        break
                }
                if err != nil {
                        return fmt.Errorf("error reading MFT record: %v", err)
                }

                // Try to parse the record
                record, err := mft.ParseRecord(buffer)
                if err != nil {
                        // Skip invalid records
                        recordNumber++
                        continue
                }

                // Extract file name attribute if present
                fileNameAttrs := record.FindAttributes(mft.AttributeTypeFileName)
                hasFileName := len(fileNameAttrs) > 0
                fileName := ""
                var fileNameData mft.FileName
                var parentRecordNumber uint64
                var parentSequenceNumber uint16
                var creation, lastModified, mftLastModified, lastAccess time.Time
                var allocatedSize, actualSize uint64
                var fileAttributes mft.FileAttribute

                if hasFileName {
                        fileNameData, err = mft.ParseFileName(fileNameAttrs[0].Data)
                        if err == nil {
                                fileName = fileNameData.Name
                                parentRecordNumber = fileNameData.ParentFileReference.RecordNumber
                                parentSequenceNumber = fileNameData.ParentFileReference.SequenceNumber
                                creation = fileNameData.Creation
                                lastModified = fileNameData.FileLastModified
                                mftLastModified = fileNameData.MftLastModified
                                lastAccess = fileNameData.LastAccess
                                allocatedSize = fileNameData.AllocatedSize
                                actualSize = fileNameData.ActualSize
                                fileAttributes = fileNameData.Flags
                        }
                }

                // Write record to CSV
                row := []string{
                        fmt.Sprintf("%d", record.FileReference.RecordNumber),
                        fmt.Sprintf("%d", record.FileReference.SequenceNumber),
                        fmt.Sprintf("%t", record.Flags.Is(mft.RecordFlagInUse)),
                        fmt.Sprintf("%t", record.Flags.Is(mft.RecordFlagIsDirectory)),
                        fmt.Sprintf("%t", hasFileName),
                        fileName,
                        fmt.Sprintf("%d", parentRecordNumber),
                        fmt.Sprintf("%d", parentSequenceNumber),
                        creation.Format(time.RFC3339),
                        lastModified.Format(time.RFC3339),
                        mftLastModified.Format(time.RFC3339),
                        lastAccess.Format(time.RFC3339),
                        fmt.Sprintf("%d", allocatedSize),
                        fmt.Sprintf("%d", actualSize),
                        fmt.Sprintf("%d", fileAttributes),
                }
                if err := writer.Write(row); err != nil {
                        return fmt.Errorf("error writing CSV row: %v", err)
                }

                recordNumber++
                if recordNumber%1000 == 0 {
                        printVerbose("Processed %d records...\n", recordNumber)
                }
        }

        printVerbose("CSV generation complete. Processed %d records.\n", recordNumber)
        return nil
}
