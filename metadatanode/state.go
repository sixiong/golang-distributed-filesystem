package metadatanode

import (
	"log"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nu7hatch/gouuid"
	"github.com/dotcloud/docker/pkg/namesgenerator"

	. "github.com/michaelmaltese/golang-distributed-filesystem/comm"
)

type replicationIntent struct {
	startedAt time.Time
	sentCommand bool
	block BlockID
	availableFrom []NodeID
	forwardTo []NodeID
}

type ReplicationIntents struct {
	intents []*replicationIntent
}

func (self *ReplicationIntents) Add(block BlockID, from []NodeID, to []NodeID) {
	if self.InProgress(block) {
		log.Fatalln("Already replicating block '" + string(block))
	}
	self.intents = append(self.intents, &replicationIntent{time.Now(), false, block, from, to})
}

func (self *ReplicationIntents) Count(node NodeID) int {
	count := 0
	for _, intent := range self.intents {
		for _, n := range intent.forwardTo {
			if n == node {
				if time.Since(intent.startedAt) < 20 * time.Second {
					count++
				}
			}
		}
	}
	return count
}

func (self *ReplicationIntents) Get(node NodeID) map[BlockID][]NodeID {
	actions := map[BlockID][]NodeID{}
	for _, intent := range self.intents {
		if intent.sentCommand {
			continue
		}
		for _, n := range intent.availableFrom {
			if n == node {
				actions[intent.block] = intent.forwardTo
				intent.sentCommand = true
				intent.startedAt = time.Now()
			}
		}
	}
	return actions
}

func (self *ReplicationIntents) Done(node NodeID, block BlockID) {
	for i, intent := range self.intents {
		if intent.block != block {
			continue
		}
		for j, n := range intent.forwardTo {
			if n == node {
				intent.forwardTo = append(intent.forwardTo[:j], intent.forwardTo[j+1:]...)
			}
		}
		if len(intent.forwardTo) == 0 {
			self.intents = append(self.intents[:i], self.intents[i+1:]...)
		}
	}
}

func (self *ReplicationIntents) InProgress(block BlockID) bool {
	for i, intent := range self.intents {
		if intent.block == block {
			if time.Since(intent.startedAt) < 20 * time.Second {
				return true
			}
			self.intents = append(self.intents[:i], self.intents[i+1:]...)
		}
	}
	return false
}

type deletionIntent struct {
	startedAt time.Time
	sentCommand bool
	block BlockID
	node NodeID
}

type DeletionIntents struct {
	intents []*deletionIntent
}

func (self *DeletionIntents) Add(block BlockID, from []NodeID) {
	for _, node := range from {
		if self.InProgress(block) {
			log.Fatalln("Already deleting block '" + string(block) + "' from '" + string(node) + "'")
		}
		self.intents = append(self.intents, &deletionIntent{time.Now(), false, block, node})
	}
}

func (self *DeletionIntents) Count(node NodeID) int {
	var deletions []BlockID
	for _, intent := range self.intents {
		if intent.node == node {
			if time.Since(intent.startedAt) < 20 * time.Second {
				deletions = append(deletions, intent.block)
			}
		}
	}
	return len(deletions)
}

func (self *DeletionIntents) Get(node NodeID) []BlockID {
	var deletions []BlockID
	for _, intent := range self.intents {
		if intent.sentCommand {
			continue
		}
		if intent.node == node {
			deletions = append(deletions, intent.block)
			intent.sentCommand = true
			intent.startedAt = time.Now()
		}
	}
	return deletions
}

func (self *DeletionIntents) Done(node NodeID, block BlockID) {
	for i, intent := range self.intents {
		if intent.node == node && intent.block == block {
			self.intents = append(self.intents[:i], self.intents[i+1:]...)
		}
	}
}

func (self *DeletionIntents) InProgress(block BlockID) bool {
	for i, intent := range self.intents {
		if intent.block == block {
			if time.Since(intent.startedAt) < 20 * time.Second {
				return true
			}
			self.intents = append(self.intents[:i], self.intents[i+1:]...)
		}
	}
	return false
}

type MetaDataNodeState struct {
	mutex sync.RWMutex
	store *DB
	dataNodes map[NodeID]string
	dataNodesLastSeen map[NodeID]time.Time
	dataNodesUtilization map[NodeID]int
	blocks map[BlockID]map[NodeID]bool
	dataNodesBlocks map[NodeID]map[BlockID]bool
	replicationIntents ReplicationIntents
	deletionIntents DeletionIntents
	ReplicationFactor int
}

func NewMetaDataNodeState() *MetaDataNodeState {
	var self MetaDataNodeState
	db, err := OpenDB("metadata.db")
	log.Println("Persistent storage at", "metadata.db")
	if err != nil {
		log.Fatalln("Metadata store error:", err)
	}
	self.store = db

	self.dataNodesLastSeen = map[NodeID]time.Time{}
	self.dataNodes = map[NodeID]string{}
	self.dataNodesUtilization = map[NodeID]int{}
	self.blocks = map[BlockID]map[NodeID]bool{}
	self.dataNodesBlocks = map[NodeID]map[BlockID]bool{}
	return &self
}

func (self *MetaDataNodeState) GenerateBlobId() string {
	u4, err := uuid.NewV4()
	if err != nil {
		log.Fatal(err)
	}
	return u4.String()
}

func (self *MetaDataNodeState) GenerateBlock(blob string) ForwardBlock {
	u4, err := uuid.NewV4()
	if err != nil {
		log.Fatalln(err)
	}
	block := BlockID(blob + ":" + u4.String())

	nodes := self.LeastUsedNodes()
	var forwardTo []NodeID
	if len(nodes) < self.ReplicationFactor {
		forwardTo = nodes
	} else {
		forwardTo = nodes[0:self.ReplicationFactor]
	}
	var addrs []string
	for _, nodeID := range forwardTo {
		addrs = append(addrs, self.dataNodes[nodeID])
	}

	// Lock?
	self.replicationIntents.Add(block, nil, forwardTo)
	return ForwardBlock{block, addrs, 128 * 1024 * 1024}
}

func (self *MetaDataNodeState) GetBlob(blobID string) []BlockID {
	self.mutex.RLock()
	defer self.mutex.RUnlock()

	names, err := self.store.Get(blobID)
	if err != nil {
		log.Fatalln(err)
	}

	// Is this really necessary?
	var blocks []BlockID
	for _, n := range names {
		blocks = append(blocks, BlockID(n))
	}

	return blocks
}

func (self *MetaDataNodeState) HasBlocks(nodeID NodeID, blocks []BlockID) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	for _, blockID := range blocks {
		self.replicationIntents.Done(nodeID, blockID)
		if self.blocks[blockID] == nil {
			self.blocks[blockID] = map[NodeID]bool{}
		}
		if self.dataNodesBlocks[nodeID] == nil {
			self.dataNodesBlocks[nodeID] = map[BlockID]bool{}
		}
		self.blocks[blockID][nodeID] = true
		self.dataNodesBlocks[nodeID][blockID] = true
	}
}

func (self *MetaDataNodeState) DoesntHaveBlocks(nodeID NodeID, blockIDs []BlockID) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	for _, blockID := range blockIDs {
		self.deletionIntents.Done(nodeID, blockID)
		if self.blocks[blockID] != nil {
			delete(self.blocks[blockID], nodeID)
		}
		if self.dataNodesBlocks[nodeID] != nil {
			delete(self.dataNodesBlocks[nodeID], blockID)
		}
	}
}

func (self *MetaDataNodeState) GetBlock(blockID BlockID) []string {
	self.mutex.RLock()
	defer self.mutex.RUnlock()

	var addrs []string
	for nodeID, _ := range self.blocks[blockID] {
		addrs = append(addrs, self.dataNodes[nodeID])
	}

	return addrs
}

func (self *MetaDataNodeState) RegisterDataNode(addr string, blocks []BlockID) NodeID {
	name := strings.Replace(namesgenerator.GetRandomName(0), "_", "-", -1)
	nodeID := NodeID(name)
	self.HasBlocks(nodeID, blocks)

	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.dataNodes[nodeID] = addr
	self.dataNodesUtilization[nodeID] = len(blocks)
	self.dataNodesLastSeen[nodeID] = time.Now()

	return nodeID
}

func (self *MetaDataNodeState) HeartbeatFrom(nodeID NodeID, utilization int) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if len(self.dataNodes[nodeID]) > 0 {
		self.dataNodesLastSeen[nodeID] = time.Now()
		self.dataNodesUtilization[nodeID] = utilization
		return true
	}
	return false
}

type ByRandom []NodeID
func (s ByRandom) Len() int {
    return len(s)
}
func (s ByRandom) Swap(i, j int) {
    s[i], s[j] = s[j], s[i]
}
func (s ByRandom) Less(i, j int) bool {
    return rand.Intn(2) == 0 // 0 or 1
}

type ByUtilization []NodeID
func (s ByUtilization) Len() int {
    return len(s)
}
func (s ByUtilization) Swap(i, j int) {
    s[i], s[j] = s[j], s[i]
}
func (s ByUtilization) Less(i, j int) bool {
    return State.Utilization(s[i]) < State.Utilization(s[j])
}

func (self *MetaDataNodeState) Utilization(n NodeID) int {
	return State.dataNodesUtilization[n] + State.replicationIntents.Count(n) - State.deletionIntents.Count(n)
}

// This is not concurrency safe
func (self *MetaDataNodeState) LeastUsedNodes() []NodeID {
	var nodes []NodeID
	for nodeID, _ := range self.dataNodes {
		nodes = append(nodes, nodeID)
	}

	sort.Sort(ByRandom(nodes))
	sort.Stable(ByUtilization(nodes))

	return nodes
}
func (self *MetaDataNodeState) MostUsedNodes() []NodeID {
	var nodes []NodeID
	for nodeID, _ := range self.dataNodes {
		nodes = append(nodes, nodeID)
	}

	sort.Sort(ByRandom(nodes))
	sort.Stable(sort.Reverse(ByUtilization(nodes)))

	return nodes
}

func (self *MetaDataNodeState) CommitBlob(name string, blocks []BlockID) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, b := range blocks {
		self.store.Append(name, string(b))
	}
}


func monitor() {
	for {
		log.Println("Monitor checking system..")
		// This sucks. Probably could do a separate lock for DataNodes and file stuff
		State.mutex.Lock()
		for id, lastSeen := range State.dataNodesLastSeen {
			if time.Since(lastSeen) > 10 * time.Second {
				log.Println("Forgetting absent node:", id)
				delete(State.dataNodesLastSeen, id)
				delete(State.dataNodes, id)
				delete(State.dataNodesUtilization, id)
				for block, _ := range State.dataNodesBlocks[id] {
					delete(State.blocks[block], id)
				}
				delete(State.dataNodesBlocks, id)
			}
		}

		for blockID, nodes := range State.blocks {
			switch {
			default:
				continue

			case State.replicationIntents.InProgress(blockID):
				continue

			case State.deletionIntents.InProgress(blockID):
				continue

			case len(nodes) > State.ReplicationFactor:
				log.Println("Block '" + blockID + "' is over-replicated")
				var deleteFrom []NodeID
				nodesByUtilization := State.MostUsedNodes()
				for _, nodeID := range nodesByUtilization {
					if len(nodes) - len(deleteFrom) <= State.ReplicationFactor {
						break
					}
					if nodes[nodeID] {
						deleteFrom = append(deleteFrom, nodeID)
					}
				}
				log.Printf("Deleting from: %v", deleteFrom)
				State.deletionIntents.Add(blockID, deleteFrom)

			case len(nodes) < State.ReplicationFactor:
				log.Println("Block '" + blockID + "' is under-replicated!")
				var forwardTo []NodeID
				nodesByUtilization := State.LeastUsedNodes()
				for _, nodeID := range nodesByUtilization {
					if len(forwardTo) + len(nodes) >= State.ReplicationFactor {
						break
					}
					if ! nodes[nodeID] {
						forwardTo = append(forwardTo, nodeID)
					}
				}
				log.Printf("Replicating to: %v", forwardTo)
				var availableFrom []NodeID
				for n, _ := range nodes {
					availableFrom = append(availableFrom, n)
				}
				State.replicationIntents.Add(blockID, availableFrom, forwardTo)
			}
		}

		if len(State.dataNodes) != 0 {
			totalUtilization := 0
			for _, utilization := range State.dataNodesUtilization {
				totalUtilization += utilization
			}
			avgUtilization := totalUtilization / len(State.dataNodes)

			var lessThanAverage []NodeID
			var moreThanAverage []NodeID
			for node, utilization := range State.dataNodesUtilization {
				switch {
				case utilization < avgUtilization:
					lessThanAverage = append(lessThanAverage, node)
				case utilization > avgUtilization:
					moreThanAverage = append(moreThanAverage, node)
				}
			}

			moveIntents := map[NodeID]int{}
			for len(lessThanAverage) != 0 && len(moreThanAverage) != 0 {
				sort.Sort(ByRandom(lessThanAverage))
				sort.Stable(ByUtilization(lessThanAverage))
				sort.Sort(ByRandom(moreThanAverage))
				sort.Stable(sort.Reverse(ByUtilization(moreThanAverage)))

				Nodes:
				for _, lessNode := range lessThanAverage {
					Blocks:
					for block, _ := range State.dataNodesBlocks[moreThanAverage[0]] {
						for existingBlock, _ := range State.dataNodesBlocks[lessNode] {
							if block == existingBlock {
								continue Blocks
							}
						}

						switch {
						case State.replicationIntents.InProgress(block):
							continue Blocks

						case State.deletionIntents.InProgress(block):
							continue Blocks

						default:
							var nodes []NodeID
							for n, _ := range State.blocks[block] {
								nodes = append(nodes, n)
							}
							log.Println("Move a block from", moreThanAverage[0], "to", lessNode)
							State.replicationIntents.Add(block, nodes, []NodeID{lessNode})
							if State.Utilization(lessThanAverage[0]) >= avgUtilization {
								lessThanAverage = lessThanAverage[1:]
							}
							break Nodes
						}
					}
				}

				// Prevent infinite loop
				moveIntents[moreThanAverage[0]]++
				State.dataNodesUtilization[moreThanAverage[0]]--
				if State.Utilization(moreThanAverage[0]) <= avgUtilization {
					moreThanAverage = moreThanAverage[1:]
				}
			}

			// So I don't have to write another sorter
			for node, offset := range moveIntents {
				State.dataNodesUtilization[node] += offset
			}
		}

		State.mutex.Unlock()

		time.Sleep(3 * time.Second)
	}
}