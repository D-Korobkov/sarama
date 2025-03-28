//go:build !functional

package sarama

import (
	"fmt"
	"testing"
	"time"
)

func makeProduceSet() (*asyncProducer, *produceSet) {
	conf := NewTestConfig()
	txnmgr, _ := newTransactionManager(conf, nil)
	parent := &asyncProducer{
		conf:   conf,
		txnmgr: txnmgr,
	}
	return parent, newProduceSet(parent)
}

func safeAddMessage(t *testing.T, ps *produceSet, msg *ProducerMessage) {
	if err := ps.add(msg); err != nil {
		t.Error(err)
	}
}

func TestProduceSetInitial(t *testing.T) {
	_, ps := makeProduceSet()

	if !ps.empty() {
		t.Error("New produceSet should be empty")
	}

	if ps.readyToFlush() {
		t.Error("Empty produceSet must never be ready to flush")
	}
}

func TestProduceSetAddingMessages(t *testing.T) {
	_, ps := makeProduceSet()
	msg := &ProducerMessage{Key: StringEncoder(TestMessage), Value: StringEncoder(TestMessage)}

	safeAddMessage(t, ps, msg)

	if ps.empty() {
		t.Error("set shouldn't be empty when a message is added")
	}

	if !ps.readyToFlush() {
		t.Error("by default set should be ready to flush when any message is in place")
	}
}

func TestProduceSetAddingMessagesOverflowMessagesLimit(t *testing.T) {
	parent, ps := makeProduceSet()
	parent.conf.Producer.Flush.MaxMessages = 1000

	msg := &ProducerMessage{Key: StringEncoder(TestMessage), Value: StringEncoder(TestMessage)}

	for i := 0; i < 1000; i++ {
		if ps.wouldOverflow(msg) {
			t.Error("set shouldn't fill up after only", i+1, "messages")
		}
		safeAddMessage(t, ps, msg)
	}

	if !ps.wouldOverflow(msg) {
		t.Error("set should be full after 1000 messages")
	}
}

func TestProduceSetAddingMessagesOverflowBytesLimit(t *testing.T) {
	parent, ps := makeProduceSet()
	parent.conf.Producer.MaxMessageBytes = 1000

	msg := &ProducerMessage{Key: StringEncoder(TestMessage), Value: StringEncoder(TestMessage)}

	for ps.bufferBytes+msg.ByteSize(2) < parent.conf.Producer.MaxMessageBytes {
		if ps.wouldOverflow(msg) {
			t.Error("set shouldn't fill up before 1000 bytes")
		}
		safeAddMessage(t, ps, msg)
	}

	if !ps.wouldOverflow(msg) {
		t.Error("set should be full after 1000 bytes")
	}
}

func TestProduceSetPartitionTracking(t *testing.T) {
	_, ps := makeProduceSet()

	m1 := &ProducerMessage{Topic: "t1", Partition: 0}
	m2 := &ProducerMessage{Topic: "t1", Partition: 1}
	m3 := &ProducerMessage{Topic: "t2", Partition: 0}
	safeAddMessage(t, ps, m1)
	safeAddMessage(t, ps, m2)
	safeAddMessage(t, ps, m3)

	seenT1P0 := false
	seenT1P1 := false
	seenT2P0 := false

	ps.eachPartition(func(topic string, partition int32, pSet *partitionSet) {
		if len(pSet.msgs) != 1 {
			t.Error("Wrong message count")
		}

		if topic == "t1" && partition == 0 {
			seenT1P0 = true
		} else if topic == "t1" && partition == 1 {
			seenT1P1 = true
		} else if topic == "t2" && partition == 0 {
			seenT2P0 = true
		}
	})

	if !seenT1P0 {
		t.Error("Didn't see t1p0")
	}
	if !seenT1P1 {
		t.Error("Didn't see t1p1")
	}
	if !seenT2P0 {
		t.Error("Didn't see t2p0")
	}

	if len(ps.dropPartition("t1", 1)) != 1 {
		t.Error("Got wrong messages back from dropping partition")
	}

	if ps.bufferCount != 2 {
		t.Error("Incorrect buffer count after dropping partition")
	}
}

func TestProduceSetRequestBuilding(t *testing.T) {
	parent, ps := makeProduceSet()
	parent.conf.Producer.RequiredAcks = WaitForAll
	parent.conf.Producer.Timeout = 10 * time.Second

	msg := &ProducerMessage{
		Topic:     "t1",
		Partition: 0,
		Key:       StringEncoder(TestMessage),
		Value:     StringEncoder(TestMessage),
	}
	for i := 0; i < 10; i++ {
		safeAddMessage(t, ps, msg)
	}
	msg.Partition = 1
	for i := 0; i < 10; i++ {
		safeAddMessage(t, ps, msg)
	}
	msg.Topic = "t2"
	for i := 0; i < 10; i++ {
		safeAddMessage(t, ps, msg)
	}

	req := ps.buildRequest()

	if req.RequiredAcks != WaitForAll {
		t.Error("RequiredAcks not set properly")
	}

	if req.Timeout != 10000 {
		t.Error("Timeout not set properly")
	}

	if len(req.records) != 2 {
		t.Error("Wrong number of topics in request")
	}
}

func TestProduceSetCompressedRequestBuilding(t *testing.T) {
	parent, ps := makeProduceSet()
	parent.conf.Producer.RequiredAcks = WaitForAll
	parent.conf.Producer.Timeout = 10 * time.Second
	parent.conf.Producer.Compression = CompressionGZIP
	parent.conf.Version = V0_10_0_0

	msg := &ProducerMessage{
		Topic:     "t1",
		Partition: 0,
		Key:       StringEncoder(TestMessage),
		Value:     StringEncoder(TestMessage),
		Timestamp: time.Now(),
	}
	for i := 0; i < 10; i++ {
		safeAddMessage(t, ps, msg)
	}

	req := ps.buildRequest()

	if req.Version != 2 {
		t.Error("Wrong request version")
	}

	for _, msgBlock := range req.records["t1"][0].MsgSet.Messages {
		msg := msgBlock.Msg
		err := msg.decodeSet()
		if err != nil {
			t.Error("Failed to decode set from payload")
		}
		for i, compMsgBlock := range msg.Set.Messages {
			compMsg := compMsgBlock.Msg
			if compMsg.Version != 1 {
				t.Error("Wrong compressed message version")
			}
			if compMsgBlock.Offset != int64(i) {
				t.Errorf("Wrong relative inner offset, expected %d, got %d", i, compMsgBlock.Offset)
			}
		}
		if msg.Version != 1 {
			t.Error("Wrong compressed parent message version")
		}
	}
}

func TestProduceSetV3RequestBuilding(t *testing.T) {
	parent, ps := makeProduceSet()
	parent.conf.Producer.RequiredAcks = WaitForAll
	parent.conf.Producer.Timeout = 10 * time.Second
	parent.conf.Version = V0_11_0_0

	now := time.Now()
	msg := &ProducerMessage{
		Topic:     "t1",
		Partition: 0,
		Key:       StringEncoder(TestMessage),
		Value:     StringEncoder(TestMessage),
		Headers: []RecordHeader{
			{
				Key:   []byte("header-1"),
				Value: []byte("value-1"),
			},
			{
				Key:   []byte("header-2"),
				Value: []byte("value-2"),
			},
			{
				Key:   []byte("header-3"),
				Value: []byte("value-3"),
			},
		},
		Timestamp: now,
	}
	for i := 0; i < 10; i++ {
		safeAddMessage(t, ps, msg)
		msg.Timestamp = msg.Timestamp.Add(time.Second)
	}

	req := ps.buildRequest()

	if req.Version != 3 {
		t.Error("Wrong request version")
	}

	batch := req.records["t1"][0].RecordBatch
	if !batch.FirstTimestamp.Equal(now.Truncate(time.Millisecond)) {
		t.Errorf("Wrong first timestamp: %v", batch.FirstTimestamp)
	}
	if !batch.MaxTimestamp.Equal(now.Add(9 * time.Second).Truncate(time.Millisecond)) {
		t.Errorf("Wrong max timestamp: %v", batch.MaxTimestamp)
	}
	for i := 0; i < 10; i++ {
		rec := batch.Records[i]
		if rec.TimestampDelta != time.Duration(i)*time.Second {
			t.Errorf("Wrong timestamp delta: %v", rec.TimestampDelta)
		}

		if rec.OffsetDelta != int64(i) {
			t.Errorf("Wrong relative inner offset, expected %d, got %d", i, rec.OffsetDelta)
		}

		for j, h := range batch.Records[i].Headers {
			exp := fmt.Sprintf("header-%d", j+1)
			if string(h.Key) != exp {
				t.Errorf("Wrong header key, expected %v, got %v", exp, h.Key)
			}
			exp = fmt.Sprintf("value-%d", j+1)
			if string(h.Value) != exp {
				t.Errorf("Wrong header value, expected %v, got %v", exp, h.Value)
			}
		}
	}
}

func TestProduceSetIdempotentRequestBuilding(t *testing.T) {
	const pID = 1000
	const pEpoch = 1234

	config := NewTestConfig()
	config.Producer.RequiredAcks = WaitForAll
	config.Producer.Idempotent = true
	config.Version = V0_11_0_0

	parent := &asyncProducer{
		conf: config,
		txnmgr: &transactionManager{
			producerID:    pID,
			producerEpoch: pEpoch,
		},
	}
	ps := newProduceSet(parent)

	now := time.Now()
	msg := &ProducerMessage{
		Topic:     "t1",
		Partition: 0,
		Key:       StringEncoder(TestMessage),
		Value:     StringEncoder(TestMessage),
		Headers: []RecordHeader{
			{
				Key:   []byte("header-1"),
				Value: []byte("value-1"),
			},
			{
				Key:   []byte("header-2"),
				Value: []byte("value-2"),
			},
			{
				Key:   []byte("header-3"),
				Value: []byte("value-3"),
			},
		},
		Timestamp:      now,
		sequenceNumber: 123,
	}
	for i := 0; i < 10; i++ {
		safeAddMessage(t, ps, msg)
		msg.Timestamp = msg.Timestamp.Add(time.Second)
	}

	req := ps.buildRequest()

	if req.Version != 3 {
		t.Error("Wrong request version")
	}

	batch := req.records["t1"][0].RecordBatch
	if !batch.FirstTimestamp.Equal(now.Truncate(time.Millisecond)) {
		t.Errorf("Wrong first timestamp: %v", batch.FirstTimestamp)
	}
	if batch.ProducerID != pID {
		t.Errorf("Wrong producerID: %v", batch.ProducerID)
	}
	if batch.ProducerEpoch != pEpoch {
		t.Errorf("Wrong producerEpoch: %v", batch.ProducerEpoch)
	}
	if batch.FirstSequence != 123 {
		t.Errorf("Wrong first sequence: %v", batch.FirstSequence)
	}
	for i := 0; i < 10; i++ {
		rec := batch.Records[i]
		if rec.TimestampDelta != time.Duration(i)*time.Second {
			t.Errorf("Wrong timestamp delta: %v", rec.TimestampDelta)
		}

		if rec.OffsetDelta != int64(i) {
			t.Errorf("Wrong relative inner offset, expected %d, got %d", i, rec.OffsetDelta)
		}

		for j, h := range batch.Records[i].Headers {
			exp := fmt.Sprintf("header-%d", j+1)
			if string(h.Key) != exp {
				t.Errorf("Wrong header key, expected %v, got %v", exp, h.Key)
			}
			exp = fmt.Sprintf("value-%d", j+1)
			if string(h.Value) != exp {
				t.Errorf("Wrong header value, expected %v, got %v", exp, h.Value)
			}
		}
	}
}

func TestProduceSetConsistentTimestamps(t *testing.T) {
	parent, ps1 := makeProduceSet()
	ps2 := newProduceSet(parent)
	parent.conf.Producer.RequiredAcks = WaitForAll
	parent.conf.Producer.Timeout = 10 * time.Second
	parent.conf.Version = V0_11_0_0

	msg1 := &ProducerMessage{
		Topic:          "t1",
		Partition:      0,
		Key:            StringEncoder(TestMessage),
		Value:          StringEncoder(TestMessage),
		Timestamp:      time.Unix(1555718400, 500000000),
		sequenceNumber: 123,
	}
	msg2 := &ProducerMessage{
		Topic:          "t1",
		Partition:      0,
		Key:            StringEncoder(TestMessage),
		Value:          StringEncoder(TestMessage),
		Timestamp:      time.Unix(1555718400, 500900000),
		sequenceNumber: 123,
	}
	msg3 := &ProducerMessage{
		Topic:          "t1",
		Partition:      0,
		Key:            StringEncoder(TestMessage),
		Value:          StringEncoder(TestMessage),
		Timestamp:      time.Unix(1555718400, 600000000),
		sequenceNumber: 123,
	}

	safeAddMessage(t, ps1, msg1)
	safeAddMessage(t, ps1, msg3)
	req1 := ps1.buildRequest()
	if req1.Version != 3 {
		t.Error("Wrong request version")
	}
	batch1 := req1.records["t1"][0].RecordBatch
	ft1 := batch1.FirstTimestamp.Unix()*1000 + int64(batch1.FirstTimestamp.Nanosecond()/1000000)
	time1 := ft1 + int64(batch1.Records[1].TimestampDelta/time.Millisecond)

	safeAddMessage(t, ps2, msg2)
	safeAddMessage(t, ps2, msg3)
	req2 := ps2.buildRequest()
	if req2.Version != 3 {
		t.Error("Wrong request version")
	}
	batch2 := req2.records["t1"][0].RecordBatch
	ft2 := batch2.FirstTimestamp.Unix()*1000 + int64(batch2.FirstTimestamp.Nanosecond()/1000000)
	time2 := ft2 + int64(batch2.Records[1].TimestampDelta/time.Millisecond)

	if time1 != time2 {
		t.Errorf("Message timestamps do not match: %v, %v", time1, time2)
	}
}
