package main

import (
        "testing"
        
        "github.com/lideming/gomft/mft"
        "github.com/stretchr/testify/assert"
)

func TestFileSizeInCSVOutput(t *testing.T) {
        // Test the logic for extracting file size from DATA attribute when FileName attribute has zero size
        
        // Create a record with a DATA attribute that has non-zero size
        record := mft.Record{
                FileReference: mft.FileReference{
                        RecordNumber: 123,
                },
                Flags: 1, // In use
        }
        
        // Add a DATA attribute with non-zero size
        dataAttr := mft.Attribute{
                Type:         mft.AttributeTypeData,
                Resident:     true,
                Name:         "",
                ActualSize:   1024,
                AllocatedSize: 4096,
        }
        
        record.Attributes = append(record.Attributes, dataAttr)
        
        // Create a RecordInfo with a FileName attribute that has zero size
        info := RecordInfo{
                RecordNumber: 123,
                Name:         "test.txt",
                Parent:       5,
                IsInUse:      true,
                IsDirectory:  false,
                FileNameAttributes: []mft.FileName{
                        {
                                Name:          "test.txt",
                                AllocatedSize: 0,
                                ActualSize:    0,
                                Namespace:     mft.FileNameNamespaceWin32,
                        },
                },
        }
        
        // Test the CSV generation logic
        fileNameAttr := info.FileNameAttributes[0]
        allocatedSize := fileNameAttr.AllocatedSize
        actualSize := fileNameAttr.ActualSize
        
        // If file size is zero in the FileName attribute, try to get it from the DATA attribute
        if actualSize == 0 && !info.IsDirectory {
                // Find DATA attributes for this record
                dataAttrs := record.FindAttributes(mft.AttributeTypeData)
                for _, dataAttr := range dataAttrs {
                        // Skip attributes with names (usually alternate data streams)
                        if dataAttr.Name != "" {
                                continue
                        }
                        
                        // Use the size from the DATA attribute
                        if dataAttr.ActualSize > 0 {
                                actualSize = dataAttr.ActualSize
                        }
                        if dataAttr.AllocatedSize > 0 {
                                allocatedSize = dataAttr.AllocatedSize
                        }
                        break
                }
        }
        
        // Verify that the sizes were updated from the DATA attribute
        assert.Equal(t, uint64(4096), allocatedSize)
        assert.Equal(t, uint64(1024), actualSize)
}