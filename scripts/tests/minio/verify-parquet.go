package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/apache/thrift/lib/go/thrift"
	parquetgo "github.com/fraugster/parquet-go"
	"github.com/fraugster/parquet-go/parquet"
	"github.com/fraugster/parquet-go/parquetschema"
)

type nopCloserReadSeeker struct {
	io.ReadSeeker
}

func (nopCloserReadSeeker) Close() error { return nil }

func main() {
	fmt.Println("================================================================================")
	fmt.Println("Concord MinIO Parquet & Thrift 0.25 Verification Harness (Issue #3527)")
	fmt.Println("================================================================================")

	failed := false

	// Part 1: Real parquet-go reader compatibility (valid & malformed)
	if err := runRealReaderValidChecks(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: Real parquet-go reader valid check failed: %v\n", err)
		failed = true
	} else {
		fmt.Println("PASS: Real parquet-go reader valid compatibility checks succeeded.")
	}

	if err := runRealReaderMalformedChecks(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: Real parquet-go reader malformed check failed: %v\n", err)
		failed = true
	} else {
		fmt.Println("PASS: Real parquet-go reader malformed fail-closed checks succeeded.")
	}

	// Part 2: Separately labeled Thrift 0.25 primitive discriminator
	if err := runThriftPrimitiveDiscriminator(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: Thrift primitive discriminator failed: %v\n", err)
		failed = true
	} else {
		fmt.Println("PASS: Thrift 0.25 TMemoryBuffer SIZE_LIMIT discriminator succeeded.")
	}

	fmt.Println("================================================================================")
	if failed {
		fmt.Println("OVERALL VERDICT: FAIL")
		os.Exit(1)
	}
	fmt.Println("OVERALL VERDICT: ALL PASS")
}

func runRealReaderValidChecks() error {
	fmt.Println("\n--- [Part 1a] Real parquet-go valid reader compatibility ---")

	// 1. Generate in-memory Parquet file using parquet-go
	var buf bytes.Buffer
	schemaDef, err := parquetschema.ParseSchemaDefinition(`
message test_record {
	required int64 id;
	required binary name (STRING);
}
`)
	if err != nil {
		return fmt.Errorf("parse schema failed: %w", err)
	}

	fw := parquetgo.NewFileWriter(&buf,
		parquetgo.WithSchemaDefinition(schemaDef),
		parquetgo.WithCompressionCodec(parquet.CompressionCodec_SNAPPY),
	)

	expectedRows := 5
	for i := 1; i <= expectedRows; i++ {
		if err := fw.AddData(map[string]any{
			"id":   int64(i),
			"name": []byte(fmt.Sprintf("user-%d", i)),
		}); err != nil {
			return fmt.Errorf("add data failed: %w", err)
		}
	}
	if err := fw.Close(); err != nil {
		return fmt.Errorf("close writer failed: %w", err)
	}

	// 2. Read back using parquetgo.NewFileReader
	fr, err := parquetgo.NewFileReader(nopCloserReadSeeker{bytes.NewReader(buf.Bytes())})
	if err != nil {
		return fmt.Errorf("NewFileReader failed: %w", err)
	}

	if fr.NumRows() != int64(expectedRows) {
		return fmt.Errorf("expected %d rows, got %d", expectedRows, fr.NumRows())
	}
	cols := fr.Columns()
	if len(cols) != 2 {
		return fmt.Errorf("expected 2 columns, got %d", len(cols))
	}

	readCount := 0
	for {
		row, err := fr.NextRow()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("NextRow failed at row %d: %w", readCount, err)
		}
		readCount++
		idVal, ok := row["id"].(int64)
		if !ok || idVal != int64(readCount) {
			return fmt.Errorf("row %d has unexpected id: %v", readCount, row["id"])
		}
	}
	if readCount != expectedRows {
		return fmt.Errorf("read %d rows, expected %d", readCount, expectedRows)
	}

	// 3. Optional: check existing repo testdata file if present in working directory
	testdataPath := "internal/s3select/testdata/testdata.parquet"
	if f, err := os.Open(testdataPath); err == nil {
		defer f.Close()
		fr2, err := parquetgo.NewFileReader(f)
		if err != nil {
			return fmt.Errorf("NewFileReader on %s failed: %w", testdataPath, err)
		}
		if fr2.NumRows() <= 0 {
			return fmt.Errorf("%s has invalid row count: %d", testdataPath, fr2.NumRows())
		}
		fmt.Printf("  -> Verified existing testdata %s (%d rows)\n", testdataPath, fr2.NumRows())
	}

	return nil
}

func runRealReaderMalformedChecks() error {
	fmt.Println("\n--- [Part 1b] Real parquet-go malformed reader compatibility ---")

	// 1. General non-Parquet rejection smoke check; does not isolate header validation.
	nonParquet := []byte("NOT_A_PARQUET_FILE_HEADER_TEST")
	if _, err := parquetgo.NewFileReader(nopCloserReadSeeker{bytes.NewReader(nonParquet)}); err == nil {
		return errors.New("expected failure on non-Parquet input, got nil")
	}

	// 2. Truncated file (< 12 bytes footer + header)
	truncBytes := []byte("PAR1")
	if _, err := parquetgo.NewFileReader(nopCloserReadSeeker{bytes.NewReader(truncBytes)}); err == nil {
		return errors.New("expected failure on truncated file, got nil")
	}

	// 3. Invalid metadata length in footer pointing outside file
	corruptLen := make([]byte, 16)
	copy(corruptLen[0:4], "PAR1")
	binary.LittleEndian.PutUint32(corruptLen[8:12], 999999) // points outside
	copy(corruptLen[12:16], "PAR1")
	if _, err := parquetgo.NewFileReader(nopCloserReadSeeker{bytes.NewReader(corruptLen)}); err == nil {
		return errors.New("expected failure on corrupted footer length, got nil")
	}

	// 4. Valid PAR1 header and footer, but malformed Thrift metadata bytes in footer
	// Declared schema count 33 with insufficient payload in StreamTransport
	malformedThrift, _ := hex.DecodeString("150219fc21")
	metaLen := uint32(len(malformedThrift))
	var malformedBuf bytes.Buffer
	malformedBuf.WriteString("PAR1")
	malformedBuf.Write(malformedThrift)
	binary.Write(&malformedBuf, binary.LittleEndian, metaLen)
	malformedBuf.WriteString("PAR1")

	if _, err := parquetgo.NewFileReader(nopCloserReadSeeker{bytes.NewReader(malformedBuf.Bytes())}); err == nil {
		return errors.New("expected failure on malformed footer thrift metadata, got nil")
	} else {
		fmt.Printf("  -> Real reader malformed footer error: %v\n", err)
	}

	return nil
}

func runThriftPrimitiveDiscriminator() error {
	fmt.Println("\n--- [Part 2] Generated parquet.FileMetaData + TMemoryBuffer Discriminator ---")
	fmt.Println("NOTICE: The production StreamTransport path in parquet-go with empty")
	fmt.Println("TConfiguration wraps an io.LimitReader and does NOT expose known remaining")
	fmt.Println("bytes to Thrift; therefore this known-size allocation bound is demonstrated")
	fmt.Println("on TMemoryBuffer and does NOT imply a newly enforced small container/allocation")
	fmt.Println("cap on MinIO's production StreamTransport path.")

	// Test case A: Declared schema count 33 with 0 remaining bytes
	// Compact protocol encoding:
	// 15 02 : Field 1 (version), delta 1, type 5 (i32), zigzag val 1
	// 19    : Field 2 (schema list), delta 1, type 9 (list)
	// fc    : List header, high bits 0xf (size >= 15), low bits 0xc (struct elem)
	// 21    : Varint length 33 (0x21)
	payloadA, err := hex.DecodeString("150219fc21")
	if err != nil {
		return fmt.Errorf("decode hex payloadA: %w", err)
	}

	bufA := thrift.NewTMemoryBuffer()
	if _, err := bufA.Write(payloadA); err != nil {
		return fmt.Errorf("write payloadA to TMemoryBuffer: %w", err)
	}

	protA := thrift.NewTCompactProtocolConf(bufA, &thrift.TConfiguration{})
	metaA := parquet.NewFileMetaData()
	errA := metaA.Read(context.Background(), protA)

	if errA == nil {
		return errors.New("discriminator case A: expected error on 0 remaining bytes, got nil")
	}

	var protoErrA thrift.TProtocolException
	if !errors.As(errA, &protoErrA) {
		return fmt.Errorf("discriminator case A: expected thrift.TProtocolException, got %T: %v", errA, errA)
	}
	if protoErrA.TypeId() != thrift.SIZE_LIMIT {
		return fmt.Errorf("discriminator case A: expected SIZE_LIMIT (TypeId=%d), got TypeId=%d: %v", thrift.SIZE_LIMIT, protoErrA.TypeId(), errA)
	}
	fmt.Printf("  -> Case A (0 remaining bytes): Got expected Thrift SIZE_LIMIT (TypeId=%d): %v\n", protoErrA.TypeId(), errA)

	// Test case B: Declared schema count 33 with only 1 schema element present (7 bytes remaining instead of required 33)
	// 150219fc21 + 48 04 63 6f 6c 30 00 (col0 element)
	payloadB, err := hex.DecodeString("150219fc214804636f6c3000")
	if err != nil {
		return fmt.Errorf("decode hex payloadB: %w", err)
	}

	bufB := thrift.NewTMemoryBuffer()
	if _, err := bufB.Write(payloadB); err != nil {
		return fmt.Errorf("write payloadB to TMemoryBuffer: %w", err)
	}

	protB := thrift.NewTCompactProtocolConf(bufB, &thrift.TConfiguration{})
	metaB := parquet.NewFileMetaData()
	errB := metaB.Read(context.Background(), protB)

	if errB == nil {
		return errors.New("discriminator case B: expected error on insufficient remaining bytes, got nil")
	}

	var protoErrB thrift.TProtocolException
	if !errors.As(errB, &protoErrB) {
		return fmt.Errorf("discriminator case B: expected thrift.TProtocolException, got %T: %v", errB, errB)
	}
	if protoErrB.TypeId() != thrift.SIZE_LIMIT {
		return fmt.Errorf("discriminator case B: expected SIZE_LIMIT (TypeId=%d), got TypeId=%d: %v", thrift.SIZE_LIMIT, protoErrB.TypeId(), errB)
	}
	fmt.Printf("  -> Case B (insufficient remaining bytes): Got expected Thrift SIZE_LIMIT (TypeId=%d): %v\n", protoErrB.TypeId(), errB)

	return nil
}
