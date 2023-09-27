// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sqldb

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	pbv2 "github.com/datacommonsorg/mixer/internal/proto/v2"
	"github.com/datacommonsorg/mixer/internal/server/resource"
	"github.com/datacommonsorg/mixer/internal/util"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var triplesInitData = []*triple{
	{subjectID: "dc/g/New", predicate: "typeOf", objectID: "StatVarGroup"},
	{subjectID: "dc/g/New", predicate: "name", objectValue: "New Variables"},
	{subjectID: "dc/g/New", predicate: "specializationOf", objectID: "dc/g/Root"},
}

type observation struct {
	entity     string
	variable   string
	date       string
	value      string
	provenance string
}

type triple struct {
	subjectID   string
	predicate   string
	objectID    string
	objectValue string
}

type csvHandle struct {
	f     io.Reader
	name  string
	close func()
}

// Write writes raw CSV files to SQLite CSV files.
func Write(sqlClient *sql.DB, resourceMetadata *resource.Metadata) error {
	fileDir := resourceMetadata.SQLDataPath
	csvFiles, err := listCSVFiles(fileDir)
	if err != nil {
		return err
	}
	if len(csvFiles) == 0 {
		return status.Errorf(codes.FailedPrecondition, "No CSV files found in %s", fileDir)
	}
	variableSet := map[string]struct{}{}
	for _, csvFile := range csvFiles {
		provID := fmt.Sprintf("dc/custom/%s", strings.TrimRight(csvFile.name, ".csv"))
		observations, variables, err := processCSVFile(resourceMetadata, csvFile, provID)
		csvFile.close()
		if err != nil {
			return err
		}
		err = writeObservations(sqlClient, observations)
		if err != nil {
			return err
		}
		err = writeTriples(sqlClient, []*triple{
			{
				subjectID: provID,
				predicate: "dcid",
				objectID:  provID,
			},
			{
				subjectID: provID,
				predicate: "typeOf",
				objectID:  "Provenance",
			},
			{
				subjectID:   provID,
				predicate:   "url",
				objectValue: filepath.Join(fileDir, csvFile.name),
			},
		})
		if err != nil {
			return err
		}
		for _, v := range variables {
			variableSet[v] = struct{}{}
		}
	}
	// Write stat var hierachy
	tripleList := triplesInitData
	for variable := range variableSet {
		tripleList = append(tripleList,
			&triple{
				subjectID: variable,
				predicate: "typeOf",
				objectID:  "StatisticalVariable",
			},
			&triple{
				subjectID: variable,
				predicate: "memberOf",
				objectID:  "dc/g/New",
			},
			&triple{
				subjectID:   variable,
				predicate:   "description",
				objectValue: variable,
			},
		)
	}
	return writeTriples(sqlClient, tripleList)
}

// Returns bucket name and object prefix string
func parseGCSPath(gcsPath string) (string, string, bool) {
	if body, ok := strings.CutPrefix(gcsPath, "gs://"); ok {
		parts := strings.SplitN(body, "/", 2)
		if len(parts) == 0 {
			return "", "", false
		}
		bucketName := parts[0]
		objectPrefix := ""
		if len(parts) == 2 {
			objectPrefix = parts[1]
			if !strings.HasPrefix(objectPrefix, "/") {
				objectPrefix += "/"
			}
		}
		log.Printf("bucket: %s, prefix: %s", bucketName, objectPrefix)
		return bucketName, objectPrefix, true
	}
	return "", "", false
}

// Get csv file handle.
// Make sure to close the file returned from this function.
func listCSVFiles(dir string) ([]*csvHandle, error) {
	var res []*csvHandle
	if bucketName, objectPrefix, ok := parseGCSPath(dir); ok {
		// Read from GCS
		ctx := context.Background()
		client, err := storage.NewClient(ctx)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		bucket := client.Bucket(bucketName)
		query := &storage.Query{
			Prefix: objectPrefix,
		}
		it := bucket.Objects(ctx, query)
		for {
			objAttrs, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return nil, err
			}
			// Check if it's a CSV file
			if strings.HasSuffix(objAttrs.Name, ".csv") {
				rc, err := bucket.Object(objAttrs.Name).NewReader(ctx)
				if err != nil {
					return nil, err
				}
				log.Printf("Added csv: %s", objAttrs.Name)
				res = append(
					res,
					&csvHandle{
						f:     rc,
						name:  objAttrs.Name,
						close: func() { rc.Close() },
					},
				)
			}
		}
	} else {
		// Read from local files
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if fName := file.Name(); strings.HasSuffix(fName, ".csv") {
				f, err := os.Open(filepath.Join(dir, fName))
				if err != nil {
					return nil, err
				}
				res = append(
					res,
					&csvHandle{
						f:     f,
						name:  fName,
						close: func() { f.Close() },
					},
				)
			}
		}
	}
	return res, nil
}

func processCSVFile(
	medatata *resource.Metadata,
	ch *csvHandle,
	provID string,
) (
	[]*observation,
	[]string, // A list of variables.
	error,
) {
	records, err := csv.NewReader(ch.f).ReadAll()
	if err != nil {
		return nil, nil, err
	}
	numRecords := len(records)
	if numRecords < 2 {
		return nil, nil, status.Errorf(codes.FailedPrecondition,
			"Empty CSV file %s", provID)
	}

	// Load header.
	header := records[0]
	if len(header) < 3 {
		return nil, nil, status.Errorf(codes.FailedPrecondition,
			"Less than 3 columns in CSV file %s", provID)
	}
	numColumns := len(header)

	// Resolve places.
	places := []string{}
	for i := 1; i < numRecords; i++ {
		places = append(places, records[i][0])
	}
	resolvedPlaceMap, err := resolvePlaces(medatata, places, header[0])
	if err != nil {
		return nil, nil, err
	}

	// Generate observations.
	observations := []*observation{}
	for i := 1; i < numRecords; i++ {
		record := records[i]

		resolvedPlace, ok := resolvedPlaceMap[record[0]]
		if !ok {
			// If a place cannot be resolved, simply ignore it.
			continue
		}

		for j := 2; j < numColumns; j++ {
			observations = append(observations, &observation{
				entity:     resolvedPlace,
				variable:   header[j],
				date:       record[1],
				value:      record[j],
				provenance: provID,
			})
		}
	}

	return observations, header[2:], nil
}

func resolvePlaces(
	metadata *resource.Metadata,
	places []string,
	placeHeader string,
) (map[string]string, error) {
	var property string
	placeToDCID := map[string]string{}
	if placeHeader == "dcid" {
		for _, place := range places {
			placeToDCID[place] = place
		}
		return placeToDCID, nil
	}
	if placeHeader == "lat#lng" {
		for _, place := range places {
			if err := validateLatLng(place); err != nil {
				return nil, err
			}
		}
		property = "<-geoCoordinate->dcid"
	} else if placeHeader == "name" {
		property = "<-description->dcid"
	} else {
		property = fmt.Sprintf("<-%s->dcid", placeHeader)
	}
	resp := &pbv2.ResolveResponse{}
	if err := util.FetchRemote(metadata, &http.Client{}, "/v2/resolve",
		&pbv2.ResolveRequest{
			Nodes:    places,
			Property: property,
		}, resp); err != nil {
		return nil, err
	}
	for _, entity := range resp.GetEntities() {
		if _, ok := placeToDCID[entity.GetNode()]; ok {
			continue
		}
		if len(entity.GetCandidates()) > 0 {
			// The resolve API sorts candidates already, so we pick the first one.
			placeToDCID[entity.GetNode()] = entity.GetCandidates()[0].GetDcid()
		}
	}
	return placeToDCID, nil
}

func validateLatLng(latLng string) error {
	parts := strings.Split(latLng, "#")
	if len(parts) != 2 {
		return status.Errorf(codes.InvalidArgument,
			"Wrong coordinate argument %s, should be latitude#longitude.", latLng)
	}

	latStr, lngStr := parts[0], parts[1]

	lat, err := strconv.ParseFloat(latStr, 64)
	if err != nil {
		return err
	}
	if lat > 90 || lat < -90 {
		return status.Errorf(codes.InvalidArgument,
			"Wrong latitude for %s", latLng)
	}

	lng, err := strconv.ParseFloat(lngStr, 64)
	if err != nil {
		return err
	}
	if lng > 180 || lng < -180 {
		return status.Errorf(codes.InvalidArgument,
			"Wrong longitude for %s", latLng)
	}

	return nil
}

func writeObservations(
	sqlClient *sql.DB,
	observations []*observation,
) error {
	for _, o := range observations {
		sqlStmt := `INSERT INTO observations(entity,variable,date,value,provenance) VALUES (?, ?, ?, ?, ?)`
		_, err := sqlClient.Exec(sqlStmt, o.entity, o.variable, o.date, o.value, o.provenance)
		if err != nil {
			return err
		}
	}
	return nil
}

func writeTriples(
	sqlClient *sql.DB,
	triples []*triple,
) error {
	for _, t := range triples {
		sqlStmt := `INSERT INTO triples(subject_id,predicate,object_id,object_value) VALUES (?, ?, ?, ?)`
		_, err := sqlClient.Exec(sqlStmt, t.subjectID, t.predicate, t.objectID, t.objectValue)
		if err != nil {
			return err
		}
	}
	return nil
}