/*
	Copyright (C) 2024  Pagefault Games

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU Affero General Public License as published by
	the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU Affero General Public License for more details.

	You should have received a copy of the GNU Affero General Public License
	along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package db

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"os"
	"strconv"

	"github.com/klauspost/compress/zstd"
	"github.com/pagefaultgames/rogueserver/defs"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TryAddSeedCompletion(uuid []byte, seed string, mode int) (bool, error) {
	var count int
	err := handle.QueryRow("SELECT COUNT(*) FROM dailyRunCompletions WHERE uuid = ? AND seed = ?", uuid, seed).Scan(&count)
	if err != nil {
		return false, err
	} else if count > 0 {
		return false, nil
	}

	_, err = handle.Exec("INSERT INTO dailyRunCompletions (uuid, seed, mode, timestamp) VALUES (?, ?, ?, UTC_TIMESTAMP())", uuid, seed, mode)
	if err != nil {
		return false, err
	}

	return true, nil
}

func ReadSeedCompleted(uuid []byte, seed string) (bool, error) {
	var count int
	err := handle.QueryRow("SELECT COUNT(*) FROM dailyRunCompletions WHERE uuid = ? AND seed = ?", uuid, seed).Scan(&count)
	if err != nil {
		return false, err
	}

	return count > 0, nil
}

func isLocalInstance() bool {
	isLocal, _ := strconv.ParseBool(os.Getenv("debug"))
	return isLocal
}

func ReadSystemSaveData(uuid []byte) (defs.SystemSaveData, error) {
	var system defs.SystemSaveData
	var err error
	if isLocalInstance() {
		system, err = ReadSystemSaveDataLocal(uuid)
	} else {
		system, err = ReadSystemSaveDataS3(uuid)
	}
	if err != nil {
		return system, err
	}
	return system, nil
}

func ReadSystemSaveDataLocal(uuid []byte) (defs.SystemSaveData, error) {
	var system defs.SystemSaveData
	_, err := isSaveInLocalDb(uuid)
	if err != nil {
		return system, err
	}
	var data []byte
	err = handle.QueryRow("SELECT data FROM systemSaveData WHERE uuid = ?", uuid).Scan(&data)
	if err != nil {
		return system, err
	}
	return system, nil
}

func isSaveInLocalDb(uuid []byte) (bool, error) {
	var isLocal bool
	err := handle.QueryRow("SELECT isInLocalDb FROM accounts WHERE uuid = ?", uuid).Scan(&isLocal)
	if err != nil {
		return false, err
	}
	return isLocal, nil
}

func ReadSystemSaveDataS3(uuid []byte) (defs.SystemSaveData, error) {
	// get and return save from S3
	system, err := GetSystemSaveFromS3(uuid)
	if err == nil {
		return system, nil
	}

	// otherwise look in database and try to move it
	var data []byte
	err = handle.QueryRow("SELECT data FROM systemSaveData WHERE uuid = ?", uuid).Scan(&data)
	if err != nil {
		return system, err
	}

	dec, err := zstd.NewReader(nil)
	if err != nil {
		return system, err
	}

	defer dec.Close()

	decompressed, err := dec.DecodeAll(data, nil)
	if err == nil {
		// replace if it worked, otherwise use the original data
		data = decompressed
	}

	err = gob.NewDecoder(bytes.NewReader(data)).Decode(&system)
	if err != nil {
		return system, err
	}

	// put it in S3
	err = StoreSystemSaveData(uuid, system)
	if err != nil {
		return system, err
	}

	// delete the one in db
	err = DeleteSystemSaveData(uuid)
	if err != nil {
		return system, err
	}

	return system, nil
}

func StoreSystemSaveData(uuid []byte, data defs.SystemSaveData) error {
	cfg, _ := config.LoadDefaultConfig(context.TODO())

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(os.Getenv("AWS_ENDPOINT_URL_S3"))
	})

	username, err := FetchUsernameFromUUID(uuid)
	if err != nil {
		return err
	}

	json, err := json.Marshal(data)
	if err != nil {
		return err
	}

	_, err = client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("pokerogue-system"),
		Key:    aws.String(username),
		Body:   bytes.NewReader(json),
	})
	if err != nil {
		return err
	}

	return nil
}

func DeleteSystemSaveData(uuid []byte) error {
	_, err := handle.Exec("DELETE FROM systemSaveData WHERE uuid = ?", uuid)
	if err != nil {
		return err
	}

	return nil
}

func ReadSessionSaveData(uuid []byte, slot int) (defs.SessionSaveData, error) {
	var session defs.SessionSaveData

	var data []byte
	err := handle.QueryRow("SELECT data FROM sessionSaveData WHERE uuid = ? AND slot = ?", uuid, slot).Scan(&data)
	if err != nil {
		return session, err
	}

	dec, err := zstd.NewReader(nil)
	if err != nil {
		return session, err
	}

	defer dec.Close()

	decompressed, err := dec.DecodeAll(data, nil)
	if err == nil {
		// replace if it worked, otherwise use the original data
		data = decompressed
	}

	err = gob.NewDecoder(bytes.NewReader(data)).Decode(&session)
	if err != nil {
		return session, err
	}

	return session, nil
}

func GetLatestSessionSaveDataSlot(uuid []byte) (int, error) {
	var slot int
	err := handle.QueryRow("SELECT slot FROM sessionSaveData WHERE uuid = ? ORDER BY timestamp DESC, slot ASC LIMIT 1", uuid).Scan(&slot)
	if err != nil {
		return -1, err
	}

	return slot, nil
}

func StoreSessionSaveData(uuid []byte, data defs.SessionSaveData, slot int) error {
	var buf bytes.Buffer
	err := gob.NewEncoder(&buf).Encode(data)
	if err != nil {
		return err
	}

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return err
	}

	defer enc.Close()

	_, err = handle.Exec("REPLACE INTO sessionSaveData (uuid, slot, data, timestamp) VALUES (?, ?, ?, UTC_TIMESTAMP())", uuid, slot, enc.EncodeAll(buf.Bytes(), nil))
	if err != nil {
		return err
	}

	return nil
}

func DeleteSessionSaveData(uuid []byte, slot int) error {
	_, err := handle.Exec("DELETE FROM sessionSaveData WHERE uuid = ? AND slot = ?", uuid, slot)
	if err != nil {
		return err
	}

	return nil
}

func RetrievePlaytime(uuid []byte) (int, error) {
	var playtime int
	err := handle.QueryRow("SELECT playTime FROM accountStats WHERE uuid = ?", uuid).Scan(&playtime)
	if err != nil {
		return 0, err
	}

	return playtime, nil
}

func GetSystemSaveFromS3(uuid []byte) (defs.SystemSaveData, error) {
	var system defs.SystemSaveData

	username, err := FetchUsernameFromUUID(uuid)
	if err != nil {
		return system, err
	}

	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return system, err
	}

	client := s3.NewFromConfig(cfg)

	s3Object := s3.GetObjectInput{
		Bucket: aws.String("pokerogue-system"),
		Key:    aws.String(username),
	}

	resp, err := client.GetObject(context.TODO(), &s3Object)
	if err != nil {
		return system, err
	}

	var session defs.SystemSaveData
	err = json.NewDecoder(resp.Body).Decode(&session)
	if err != nil {
		return system, err
	}

	return session, nil
}

func GetLocalSystemAccounts() ([][]byte, error) {
	var users [][]byte
	rows, err := handle.Query("SELECT uuid FROM systemSaveData LIMIT 3000")
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	for rows.Next() {
		var uuid []byte
		err := rows.Scan(&uuid)
		if err != nil {
			return nil, err
		}

		users = append(users, uuid)
	}

	return users, nil
}
