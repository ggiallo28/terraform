package dynamodb

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	multierror "github.com/hashicorp/go-multierror"
	uuid "github.com/hashicorp/go-uuid"
	"github.com/hashicorp/terraform/state"
	"github.com/hashicorp/terraform/state/remote"
)

// Store the last saved serial in dynamo with this suffix for consistency checks.
const (
	stateIDSuffix    = "-md5"
	dynamoDBItemSize = 400000
)

type RemoteClient struct {
	dynClient        *dynamodb.DynamoDB
	dynGlobalClients []*dynamodb.DynamoDB

	tableName      string
	path           string
	lockTable      string
	state_days_ttl int
	compression    bool
}

var (
	// The amount of time we will retry a state waiting for it to match the expected checksum.
	consistencyRetryTimeout = 10 * time.Second

	// delay when polling the state
	consistencyRetryPollInterval = 2 * time.Second
)

// test hook called when checksums don't match
var testChecksumHook func()

func (c *RemoteClient) Get() (payload *remote.Payload, err error) {
	deadline := time.Now().Add(consistencyRetryTimeout)

	// If we have a checksum, and the returned payload doesn't match, we retry
	// up until deadline.
	for {
		payload, err = c.get()
		if err != nil {
			return nil, err
		}

		// If the remote state was manually removed the payload will be nil,
		// but if there's still a digest entry for that state we will still try
		// to compare the MD5 below.
		var digest []byte
		if payload != nil {
			digest = payload.MD5
		}

		// verify that this state is what we expect
		if expected, err := c.getMD5(); err != nil {
			log.Printf("[WARN] failed to fetch state md5: %s", err)
		} else if len(expected) > 0 && !bytes.Equal(expected, digest) {
			log.Printf("[WARN] state md5 mismatch: expected '%x', got '%x'", expected, digest)

			if testChecksumHook != nil {
				testChecksumHook()
			}

			if time.Now().Before(deadline) {
				time.Sleep(consistencyRetryPollInterval)
				log.Println("[INFO] retrying DynamoDB RemoteClient.Get...")
				continue
			}

			return nil, fmt.Errorf(errBadChecksumFmt, digest)
		}

		break
	}

	return payload, err
}

func (c *RemoteClient) getChunks() ([]State, bool, error) {
	var queryInput = &dynamodb.QueryInput{
		TableName: aws.String(c.tableName),
		KeyConditions: map[string]*dynamodb.Condition{
			"StateID": {
				ComparisonOperator: aws.String("EQ"),
				AttributeValueList: []*dynamodb.AttributeValue{
					{
						S: aws.String(c.path),
					},
				},
			},
		},
		Limit:            aws.Int64(1),
		ScanIndexForward: aws.Bool(false), // descending order
	}

	var states []State
	is_compressed := false
	for {
		result, err := c.dynClient.Query(queryInput)
		if err != nil {
			return nil, is_compressed, fmt.Errorf("During query operation on table %s %s.", c.tableName, err)
		}
		if len(result.Items) == 0 {
			break
		}

		state := State{}
		if err := dynamodbattribute.UnmarshalMap(result.Items[0], &state); err != nil {
			return nil, is_compressed, fmt.Errorf("Got error marshalling state: %s", err)
		}

		if result.Items[0]["Body"].S != nil {
			state.Body = []byte(state.Body.(string))
		} else {
			state.Body = state.Body.([]byte)
			is_compressed = true
		}

		states = append(states, state)

		if state.NextStateID == "" {
			break
		} else {
			queryInput.KeyConditions["StateID"].AttributeValueList[0].S = aws.String(state.NextStateID)
		}

		time.Sleep(consistencyRetryPollInterval)
	}

	for i := 0; i < len(states)-1; i += 1 {
		if states[i].VersionID != states[i+1].VersionID {
			return nil, is_compressed, fmt.Errorf("Got wrong VersionID")
		}
	}

	return states, is_compressed, nil
}

func (c *RemoteClient) decompress(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	return ioutil.ReadAll(gz)
}

func (c *RemoteClient) get() (*remote.Payload, error) {
	states, is_compressed, err := c.getChunks()
	if err != nil {
		return nil, err
	}

	var jsonString []byte
	for _, state := range states {
		jsonString = append(jsonString, state.Body.([]byte)...)
	}

	sum := md5.Sum(jsonString)
	payload := &remote.Payload{
		Data: jsonString,
		MD5:  sum[:],
	}

	// If there was no data, then return nil
	if len(payload.Data) == 0 {
		return nil, nil
	}

	if is_compressed {
		fmt.Println("Decompressing remote state. This may take a few moments...")
		payload.Data, err = c.decompress(payload.Data)
		if err != nil {
			return nil, err
		}
	}
	return payload, nil
}

func (c *RemoteClient) compress(data []byte) []byte {
	b := &bytes.Buffer{}
	gz := gzip.NewWriter(b)
	gz.Write(data)
	gz.Close()
	return b.Bytes()
}

func (c *RemoteClient) Put(data []byte) error {
	states, is_compressed, err := c.getChunks()
	if err != nil {
		return err
	}

	var segment_id int64
	if len(states) == 0 {
		segment_id = -2
	} else {
		segment_id = states[0].VersionID
	}

	version_date := time.Now().AddDate(0, 0, c.state_days_ttl).Unix()
	if c.state_days_ttl == 0 {
		version_date = 0
	}

	var transactionItems = make([]*dynamodb.TransactWriteItem, 0)

	for _, state := range states {

		delete_item := &dynamodb.TransactWriteItem{
			Delete: &dynamodb.Delete{
				TableName: aws.String(c.tableName),
				Key: map[string]*dynamodb.AttributeValue{
					"StateID": {
						S: aws.String(state.StateID),
					},
					"VersionID": {
						N: aws.String(strconv.FormatInt(state.VersionID, 10)),
					},
				},
			},
		}

		transactionItems = append(transactionItems, delete_item)

		if c.state_days_ttl >= 0 {

			var body interface{}
			if is_compressed {
				body = state.Body.([]byte)
			} else {
				body = string(state.Body.([]byte))
			}

			new_state := State{
				StateID:     state.StateID,
				VersionID:   segment_id + 1,
				Body:        body,
				NextStateID: state.NextStateID,
				TTL:         version_date,
			}

			av, err := dynamodbattribute.MarshalMap(new_state)
			if err != nil {
				return fmt.Errorf("Got error marshalling state: %s", err)
			}
			put_item := &dynamodb.TransactWriteItem{
				Put: &dynamodb.Put{
					TableName: aws.String(c.tableName),
					Item:      av,
				},
			}

			transactionItems = append(transactionItems, put_item)

		}

	}

	if c.state_days_ttl >= 0 && len(transactionItems) > 0 {
		_, err := c.dynClient.TransactWriteItems(&dynamodb.TransactWriteItemsInput{TransactItems: transactionItems})
		if err != nil {
			return fmt.Errorf("Got error calling TransactWriteItems: %s", err)
		}
		transactionItems = make([]*dynamodb.TransactWriteItem, 0)
	}

	// State GZIP Compression
	if c.compression {
		fmt.Println("Compressing remote state. This may take a few moments...")
		data = c.compress(data)
	}

	var chunks []State
	for i := 0; i < len(data); i += dynamoDBItemSize {
		end := i + dynamoDBItemSize
		if end > len(data) {
			end = len(data)
		}

		path := c.path
		if i > 0 {
			hex := strconv.FormatInt(time.Now().Unix(), 16) + strconv.FormatInt(int64(i), 16)
			path = path + "-" + hex
		}

		var body interface{}
		if c.compression {
			body = data[i:end]
		} else {
			body = string(data[i:end])
		}

		state := State{
			StateID:   path,
			VersionID: segment_id + 2,
			Body:      body,
		}

		chunks = append(chunks, state)
	}

	for i := 0; i < len(chunks)-1; i += 1 {
		chunks[i].NextStateID = chunks[i+1].StateID
	}

	for i := 0; i < len(chunks); i += 1 {
		av, err := dynamodbattribute.MarshalMap(chunks[i])
		if err != nil {
			return fmt.Errorf("Got error marshalling state: %s", err)
		}

		put_item := &dynamodb.TransactWriteItem{
			Put: &dynamodb.Put{
				TableName: aws.String(c.tableName),
				Item:      av,
			},
		}

		transactionItems = append(transactionItems, put_item)
	}

	log.Printf("[DEBUG] Uploading remote state to DynamoDB: %#v", transactionItems)

	{
		_, err := c.dynClient.TransactWriteItems(&dynamodb.TransactWriteItemsInput{TransactItems: transactionItems})
		if err != nil {
			return fmt.Errorf("Got error calling TransactWriteItems: %s", err)
		}
	}

	sum := md5.Sum(data)
	// if this errors out, we unfortunately have to error out altogether,
	// since the next Get will inevitably fail.
	if err := c.putMD5(sum[:]); err != nil {
		return fmt.Errorf("Failed to store state MD5: %s", err)
	}

	return nil
}

func (c *RemoteClient) Delete() error {
	var queryInput = &dynamodb.QueryInput{
		TableName: aws.String(c.tableName),
		KeyConditions: map[string]*dynamodb.Condition{
			"StateID": {
				ComparisonOperator: aws.String("EQ"),
				AttributeValueList: []*dynamodb.AttributeValue{
					{
						S: aws.String(c.path),
					},
				},
			},
		},
	}

	result, err := c.dynClient.Query(queryInput)
	if err != nil {
		return err
	}
	var transactionItems = make([]*dynamodb.TransactWriteItem, 0)
	for _, i := range result.Items {
		state := State{}

		err = dynamodbattribute.UnmarshalMap(i, &state)
		if err != nil {
			return fmt.Errorf("Got error marshalling state: %s", err)
		}
		delete_item := &dynamodb.TransactWriteItem{
			Delete: &dynamodb.Delete{
				TableName: aws.String(c.tableName),
				Key: map[string]*dynamodb.AttributeValue{
					"StateID": {
						S: aws.String(state.StateID),
					},
					"VersionID": {
						N: aws.String(strconv.FormatInt(state.VersionID, 10)),
					},
				},
			},
		}
		transactionItems = append(transactionItems, delete_item)
	}

	_, err = c.dynClient.TransactWriteItems(&dynamodb.TransactWriteItemsInput{TransactItems: transactionItems})
	if err != nil {
		return fmt.Errorf("Got error calling TransactWriteItems: %s", err)
	}

	if err != nil {
		return err
	}

	if err := c.deleteMD5(); err != nil {
		log.Printf("Error deleting state md5: %s", err)
	}

	return nil
}

func (c *RemoteClient) Lock(info *state.LockInfo) (string, error) {
	if c.lockTable == "" {
		log.Println("[WARN] Lock Table info not provided.")
		return "", nil
	}

	info.Path = c.lockPath()

	if info.ID == "" {
		lockID, err := uuid.GenerateUUID()
		if err != nil {
			return "", err
		}

		info.ID = lockID
	}

	putParams := &dynamodb.PutItemInput{
		Item: map[string]*dynamodb.AttributeValue{
			"LockID": {S: aws.String(c.lockPath())},
			"Info":   {S: aws.String(string(info.Marshal()))},
		},
		TableName:           aws.String(c.lockTable),
		ConditionExpression: aws.String("attribute_not_exists(LockID)"),
	}
	_, err := c.dynClient.PutItem(putParams)

	if err != nil {
		lockInfo, infoErr := c.getLockInfo()
		if infoErr != nil {
			err = multierror.Append(err, infoErr)
		}

		lockErr := &state.LockError{
			Err:  err,
			Info: lockInfo,
		}
		return "", lockErr
	}

	if len(c.dynGlobalClients) > 0 { //isGlobal
		lockInfo, err := c.getGlobalLockInfo()
		if err != nil {
			err = multierror.Append(err, fmt.Errorf(globalLockError))
		}
		if lockInfo != nil {
			lockErr := &state.LockError{
				Err:  err,
				Info: lockInfo,
			}
			return "", lockErr
		}
	}

	return info.ID, nil
}

func (c *RemoteClient) getGlobalLockInfo() (*state.LockInfo, error) {

	queryInput := &dynamodb.QueryInput{
		TableName: aws.String(c.lockTable),
		KeyConditions: map[string]*dynamodb.Condition{
			"LockID": {
				ComparisonOperator: aws.String("EQ"),
				AttributeValueList: []*dynamodb.AttributeValue{
					{
						S: aws.String(c.lockPath()),
					},
				},
			},
		},
	}

	//Wait dynamodb propagate lock
	var results []*dynamodb.QueryOutput
	for {
		results = make([]*dynamodb.QueryOutput, 0)
		for _, client := range c.dynGlobalClients {
			result, err := client.Query(queryInput)
			if err != nil {
				return nil, err
			}
			if *result.Count == 0 {
				break
			}
			results = append(results, result)
		}
		if len(results) == len(c.dynGlobalClients) {
			var regions []string
			for _, result := range results {
				if result.Items[0]["aws:rep:updateregion"] != nil {
					regions = append(regions, *result.Items[0]["aws:rep:updateregion"].S)
				} else {
					regions = append(regions, "")
				}
			}
			isLockReplicated := true
			for i := 1; i < len(regions); i++ {
				if regions[i] != regions[0] {
					isLockReplicated = false
				}
			}
			if isLockReplicated {
				break
			}
		}

		time.Sleep(consistencyRetryPollInterval)
	}

	clientRegion := *c.dynClient.Client.Config.Region
	lockRegion := *results[0].Items[0]["aws:rep:updateregion"].S
	if lockRegion != clientRegion {
		var infoData string
		if v, ok := results[0].Items[0]["Info"]; ok && v.S != nil {
			infoData = *v.S
		}
		lockInfo := &state.LockInfo{}
		err := json.Unmarshal([]byte(infoData), lockInfo)
		if err != nil {
			return nil, err
		}
		return lockInfo, nil
	}

	return nil, nil
}

func getClientMD5(client *dynamodb.DynamoDB, getParams *dynamodb.GetItemInput) ([]byte, error) {
	resp, err := client.GetItem(getParams)
	if err != nil {
		return nil, err
	}

	var val string
	if v, ok := resp.Item["Digest"]; ok && v.S != nil {
		val = *v.S
	}

	sum, err := hex.DecodeString(val)
	if err != nil || len(sum) != md5.Size {
		return nil, errors.New("invalid md5")
	}

	return sum, nil
}

func (c *RemoteClient) getMD5() ([]byte, error) {
	if c.lockTable == "" {
		log.Println("[WARN] Lock Table info not provided.")
		return nil, nil
	}

	getParams := &dynamodb.GetItemInput{
		Key: map[string]*dynamodb.AttributeValue{
			"LockID": {S: aws.String(c.lockPath() + stateIDSuffix)},
		},
		ProjectionExpression: aws.String("LockID, Digest"),
		TableName:            aws.String(c.lockTable),
		ConsistentRead:       aws.Bool(true),
	}

	if len(c.dynGlobalClients) > 0 { //isGlobal
		log.Println("[INFO] Working with Global Tables.")
		var sum []byte
		for {
			sums := make([][]byte, 0)
			for _, client := range c.dynGlobalClients {
				sum, _ = getClientMD5(client, getParams)
				sums = append(sums, sum)
			}
			isSumReplicated := true
			for _, s := range sums {
				res := bytes.Compare(s, sum)
				if res != 0 {
					isSumReplicated = false
				}
			}
			if isSumReplicated {
				break
			}

			time.Sleep(consistencyRetryPollInterval)
		}
		return sum, nil
	} else {
		sum, err := getClientMD5(c.dynClient, getParams)
		if err != nil {
			return nil, err
		}
		return sum, nil
	}
}

// store the hash of the state so that clients can check for stale state files.
func (c *RemoteClient) putMD5(sum []byte) error {
	if c.lockTable == "" {
		log.Println("[WARN] Lock Table info not provided.")
		return nil
	}

	if len(sum) != md5.Size {
		return errors.New("invalid payload md5")
	}

	putParams := &dynamodb.PutItemInput{
		Item: map[string]*dynamodb.AttributeValue{
			"LockID": {S: aws.String(c.lockPath() + stateIDSuffix)},
			"Digest": {S: aws.String(hex.EncodeToString(sum))},
		},
		TableName: aws.String(c.lockTable),
	}
	_, err := c.dynClient.PutItem(putParams)
	if err != nil {
		log.Printf("[WARN] failed to record state serial in dynamodb: %s", err)
	}

	return nil
}

// remove the hash value for a deleted state
func (c *RemoteClient) deleteMD5() error {
	if c.lockTable == "" {
		log.Println("[WARN] Lock Table info not provided.")
		return nil
	}

	params := &dynamodb.DeleteItemInput{
		Key: map[string]*dynamodb.AttributeValue{
			"LockID": {S: aws.String(c.lockPath() + stateIDSuffix)},
		},
		TableName: aws.String(c.lockTable),
	}

	if _, err := c.dynClient.DeleteItem(params); err != nil {
		return err
	}

	return nil
}

func (c *RemoteClient) getLockInfo() (*state.LockInfo, error) {
	getParams := &dynamodb.GetItemInput{
		Key: map[string]*dynamodb.AttributeValue{
			"LockID": {S: aws.String(c.lockPath())},
		},
		ProjectionExpression: aws.String("LockID, Info"),
		TableName:            aws.String(c.lockTable),
		ConsistentRead:       aws.Bool(true),
	}

	resp, err := c.dynClient.GetItem(getParams)
	if err != nil {
		return nil, err
	}

	var infoData string
	if v, ok := resp.Item["Info"]; ok && v.S != nil {
		infoData = *v.S
	}

	lockInfo := &state.LockInfo{}
	err = json.Unmarshal([]byte(infoData), lockInfo)
	if err != nil {
		return nil, err
	}

	return lockInfo, nil
}

func (c *RemoteClient) Unlock(id string) error {
	if c.lockTable == "" {
		log.Println("[WARN] Lock Table info not provided.")
		return nil
	}

	lockErr := &state.LockError{}

	lockInfo, err := c.getLockInfo()
	if err != nil {
		lockErr.Err = fmt.Errorf("failed to retrieve lock info: %s", err)
		return lockErr
	}
	lockErr.Info = lockInfo

	if lockInfo.ID != id {
		lockErr.Err = fmt.Errorf("lock id %q does not match existing lock", id)
		return lockErr
	}

	params := &dynamodb.DeleteItemInput{
		Key: map[string]*dynamodb.AttributeValue{
			"LockID": {S: aws.String(c.lockPath())},
		},
		TableName: aws.String(c.lockTable),
	}

	_, err = c.dynClient.DeleteItem(params)
	if err != nil {
		lockErr.Err = err
		return lockErr
	}

	return nil
}

func (c *RemoteClient) lockPath() string {
	return fmt.Sprintf("%s/%s", c.tableName, c.path)
}

const errBadChecksumFmt = `State data in DynamoDB does not have the expected content.

This may be caused by unusually long delays in DynamoDB processing a previous state
update.  Please wait for a minute or two and try again. If this problem
persists, and DynamoDB is not experiencing an outage, you may need
to manually verify the remote state and update the Digest value stored in the
DynamoDB lock table to the following value: %x
`

const globalLockError = `Error while trying to lock global table`
