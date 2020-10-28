package kafka

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"sort"
)

// Client is a new and experimental API for kafka-go. It is expected that this API will grow over time,
// and offer a new set of "mid-level" capabilities. Specifically, it is expected Client will be a higher level API than Conn,
// yet provide more control and lower level operations than the Reader and Writer APIs.
//
// N.B Client is currently experimental! Therefore, it is subject to change, including breaking changes
// between MINOR and PATCH releases.
type Client struct {
	brokers []string
	dialer  *Dialer
}

// Configuration for Client
//
// N.B ClientConfig is currently experimental! Therefore, it is subject to change, including breaking changes
// between MINOR and PATCH releases.
type ClientConfig struct {
	// List of broker strings in the format <host>:<port>
	// to use for bootstrap connecting to cluster
	Brokers []string
	// Dialer used for connecting to the Cluster
	Dialer *Dialer
}

// A ConsumerGroup and Topic as these are both strings
// we define a type for clarity when passing to the Client
// as a function argument
//
// N.B TopicAndGroup is currently experimental! Therefore, it is subject to change, including breaking changes
// between MINOR and PATCH releases.
type TopicAndGroup struct {
	Topic   string
	GroupId string
}

// NewClient creates and returns a *Client taking ...string of bootstrap
// brokers for connecting to the cluster.
func NewClient(brokers ...string) *Client {
	return NewClientWith(ClientConfig{Brokers: brokers, Dialer: DefaultDialer})
}

// NewClientWith creates and returns a *Client. For safety, it copies the []string of bootstrap
// brokers for connecting to the cluster and uses the user supplied Dialer.
// In the event the Dialer is nil, we use the DefaultDialer.
func NewClientWith(config ClientConfig) *Client {
	if len(config.Brokers) == 0 {
		panic("must provide at least one broker")
	}

	b := make([]string, len(config.Brokers))
	copy(b, config.Brokers)
	d := config.Dialer
	if d == nil {
		d = DefaultDialer
	}

	return &Client{
		brokers: b,
		dialer:  d,
	}
}

// ConsumerOffsets returns a map[int]int64 of partition to committed offset for a consumer group id and topic
func (c *Client) ConsumerOffsets(ctx context.Context, tg TopicAndGroup) (map[int]int64, error) {
	address, err := c.lookupCoordinator(ctx, tg.GroupId)
	if err != nil {
		return nil, err
	}

	conn, err := c.coordinator(ctx, address)
	if err != nil {
		return nil, err
	}

	defer conn.Close()
	partitions, err := conn.ReadPartitions(tg.Topic)
	if err != nil {
		return nil, err
	}

	var parts []int32
	for _, p := range partitions {
		parts = append(parts, int32(p.ID))
	}

	offsets, err := conn.offsetFetch(offsetFetchRequestV1{
		GroupID: tg.GroupId,
		Topics: []offsetFetchRequestV1Topic{
			{
				Topic:      tg.Topic,
				Partitions: parts,
			},
		},
	})

	if err != nil {
		return nil, err
	}

	if len(offsets.Responses) != 1 {
		return nil, fmt.Errorf("error fetching offsets, no responses received")
	}

	offsetsByPartition := map[int]int64{}
	for _, pr := range offsets.Responses[0].PartitionResponses {
		offset := pr.Offset
		if offset < 0 {
			// No offset stored
			// -1 indicates that there is no offset saved for the partition.
			// If we returned a -1 here the user might interpret that as LastOffset
			// so we set to Firstoffset for safety.
			// See http://kafka.apache.org/protocol.html#The_Messages_OffsetFetch
			offset = FirstOffset
		}
		offsetsByPartition[int(pr.Partition)] = offset
	}

	return offsetsByPartition, nil
}

// ConsumerGroupInfo contains a groupID and its coordinator broker.
type ConsumerGroupInfo struct {
	GroupID     string
	Coordinator int
}

// ListGroups returns info about all groups in the cluster.
func (c *Client) ListGroups(ctx context.Context) ([]ConsumerGroupInfo, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}

	brokers, err := conn.Brokers()
	if err != nil {
		return nil, err
	}

	groupInfos := []ConsumerGroupInfo{}

	// TODO: Parallelize this?
	for _, broker := range brokers {
		brokerConn, err := c.dialer.DialContext(
			ctx,
			"tcp",
			fmt.Sprintf("%s:%d", broker.Host, broker.Port),
		)
		if err != nil {
			return groupInfos, err
		}

		resp, err := brokerConn.listGroups(listGroupsRequestV0{})
		if err != nil {
			return groupInfos, err
		}

		for _, group := range resp.Groups {
			groupInfos = append(
				groupInfos,
				ConsumerGroupInfo{
					GroupID:     group.GroupID,
					Coordinator: broker.ID,
				},
			)
		}
	}

	sort.Slice(groupInfos, func(a, b int) bool {
		return groupInfos[a].GroupID < groupInfos[b].GroupID
	})

	return groupInfos, nil
}

// GroupInfo contains detailed information about a single group and its members.
type GroupInfo struct {
	ErrorCode int16
	GroupID   string
	State     string
	Members   []MemberInfo
}

// MemberInfo represents the membership information for a single group member.
type MemberInfo struct {
	MemberID          string
	ClientID          string
	ClientHost        string
	MemberMetadata    GroupMemberMetadata
	MemberAssignments GroupMemberAssignmentsInfo
}

// GroupMemberMetadata stores metadata associated with a group member.
type GroupMemberMetadata struct {
	Version  int16
	Topics   []string
	UserData []byte
}

// GroupMemberAssignmentsInfo stores the topic partition assignment data for a group member.
type GroupMemberAssignmentsInfo struct {
	Version  int16
	Topics   []GroupMemberTopic
	UserData []byte
}

// GroupMemberTopic is a mapping from a topic to a list of partitions in the topic. It is used
// to represent the topic partitions that have been assigned to a group member.
type GroupMemberTopic struct {
	Topic      string
	Partitions []int32
}

// DescribeGroup returns detailed information about a single consumer group.
func (c *Client) DescribeGroup(ctx context.Context, groupID string) (GroupInfo, error) {
	groupInfo := GroupInfo{}

	address, err := c.lookupCoordinator(ctx, groupID)
	if err != nil {
		return groupInfo, err
	}

	conn, err := c.coordinator(ctx, address)
	if err != nil {
		return groupInfo, err
	}

	req := describeGroupsRequestV0{
		GroupIDs: []string{groupID},
	}
	resp, err := conn.describeGroups(req)
	if err != nil {
		return groupInfo, err
	}

	if len(resp.Groups) != 1 {
		return groupInfo, fmt.Errorf("Got %d groups, expected 1", len(resp.Groups))
	}

	groupObj := resp.Groups[0]

	groupInfo.ErrorCode = groupObj.ErrorCode
	groupInfo.GroupID = groupObj.GroupID
	groupInfo.State = groupObj.State
	groupInfo.Members = []MemberInfo{}

	for _, member := range groupObj.Members {
		if member.MemberID == "" {
			// Skip over empty member slots
			continue
		}

		memberMetadata, err := decodeMemberMetadata(member.MemberMetadata)
		if err != nil {
			return groupInfo, err
		}

		memberAssignments, err := decodeMemberAssignments(member.MemberAssignments)
		if err != nil {
			return groupInfo, err
		}

		groupInfo.Members = append(
			groupInfo.Members,
			MemberInfo{
				MemberID:          member.MemberID,
				ClientID:          member.ClientID,
				ClientHost:        member.ClientHost,
				MemberMetadata:    memberMetadata,
				MemberAssignments: memberAssignments,
			},
		)
	}

	return groupInfo, nil
}

// decodeMemberMetadata converts raw metadata bytes to a GroupMemberMetadata struct.
func decodeMemberMetadata(rawMetadata []byte) (GroupMemberMetadata, error) {
	mm := GroupMemberMetadata{}

	if len(rawMetadata) == 0 {
		return mm, nil
	}

	buf := bytes.NewBuffer(rawMetadata)
	bufReader := bufio.NewReader(buf)
	remain := len(rawMetadata)

	var err error

	if remain, err = readInt16(bufReader, remain, &mm.Version); err != nil {
		return mm, err
	}
	if remain, err = readStringArray(bufReader, remain, &mm.Topics); err != nil {
		return mm, err
	}
	if remain, err = readBytes(bufReader, remain, &mm.UserData); err != nil {
		return mm, err
	}

	if remain != 0 {
		// Notice: not sure why, but in some situation, the remaining bytes
		//		   won't be zero. I cannot find a dcoument explaining the
		//		   format of this metadata payload explaing what it could be.
		//		   I guess it could be a new field introduced in newer version
		//		   of Kafka? Given this is causing the lag reporting fails
		//		   to function correctly, we are skipping the checks here
		//		   for now.
		//	       ref: https://github.com/segmentio/topicctl/issues/15
		// return mm, fmt.Errorf("Got non-zero number of bytes remaining: %d", remain)
	}

	return mm, nil
}

// decodeMemberAssignments converts raw assignment bytes to a GroupMemberAssignmentsInfo struct.
func decodeMemberAssignments(rawAssignments []byte) (GroupMemberAssignmentsInfo, error) {
	ma := GroupMemberAssignmentsInfo{}

	if len(rawAssignments) == 0 {
		return ma, nil
	}

	buf := bytes.NewBuffer(rawAssignments)
	bufReader := bufio.NewReader(buf)
	remain := len(rawAssignments)

	var err error

	if remain, err = readInt16(bufReader, remain, &ma.Version); err != nil {
		return ma, err
	}

	fn := func(r *bufio.Reader, size int) (fnRemain int, fnErr error) {
		item := GroupMemberTopic{
			Partitions: []int32{},
		}

		if fnRemain, fnErr = readString(r, size, &item.Topic); fnErr != nil {
			return
		}

		if fnRemain, fnErr = readInt32Array(r, fnRemain, &item.Partitions); fnErr != nil {
			return
		}

		ma.Topics = append(ma.Topics, item)
		return
	}
	if remain, err = readArrayWith(bufReader, remain, fn); err != nil {
		return ma, err
	}

	if remain, err = readBytes(bufReader, remain, &ma.UserData); err != nil {
		return ma, err
	}

	if remain != 0 {
		return ma, fmt.Errorf("Got non-zero number of bytes remaining: %d", remain)
	}

	return ma, nil
}

// readInt32Array reads an array of int32s. It's adapted from the implementation of
// readStringArray.
func readInt32Array(r *bufio.Reader, sz int, v *[]int32) (remain int, err error) {
	var content []int32
	fn := func(r *bufio.Reader, size int) (fnRemain int, fnErr error) {
		var value int32
		if fnRemain, fnErr = readInt32(r, size, &value); fnErr != nil {
			return
		}
		content = append(content, value)
		return
	}
	if remain, err = readArrayWith(r, sz, fn); err != nil {
		return
	}

	*v = content
	return
}

// connect returns a connection to ANY broker
func (c *Client) connect(ctx context.Context) (conn *Conn, err error) {
	for _, broker := range c.brokers {
		if conn, err = c.dialer.DialContext(ctx, "tcp", broker); err == nil {
			return
		}
	}
	return // err will be non-nil
}

// coordinator returns a connection to a coordinator
func (c *Client) coordinator(ctx context.Context, address string) (*Conn, error) {
	conn, err := c.dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to coordinator, %v", address)
	}

	return conn, nil
}

// lookupCoordinator scans the brokers and looks up the address of the
// coordinator for the groupId.
func (c *Client) lookupCoordinator(ctx context.Context, groupId string) (string, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return "", fmt.Errorf("unable to find coordinator to any connect for group, %v: %v\n", groupId, err)
	}
	defer conn.Close()

	out, err := conn.findCoordinator(findCoordinatorRequestV0{
		CoordinatorKey: groupId,
	})
	if err != nil {
		return "", fmt.Errorf("unable to find coordinator for group, %v: %v", groupId, err)
	}

	address := fmt.Sprintf("%v:%v", out.Coordinator.Host, out.Coordinator.Port)
	return address, nil
}
