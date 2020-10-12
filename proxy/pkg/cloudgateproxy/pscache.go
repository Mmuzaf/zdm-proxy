package cloudgateproxy

import (
	"encoding/binary"
	log "github.com/sirupsen/logrus"
	"sync"
)

type preparedStatementInfo struct {
	forwardDecision forwardDecision
}

type PreparedStatementCache struct {

	// Map containing the statement to be prepared and whether it is a read or a write by streamID
	// This is kind of transient: it only contains statements that are being prepared at the moment.
	// Once the response to the prepare request is processed, the statement is removed from this map
	statementsBeingPrepared map[uint16]preparedStatementInfo
	// Map containing the prepared queries (raw bytes) keyed on prepareId
	cache map[string]preparedStatementInfo
	lock  *sync.RWMutex
}

func NewPreparedStatementCache() *PreparedStatementCache {
	return &PreparedStatementCache{
		statementsBeingPrepared: make(map[uint16]preparedStatementInfo),
		cache:                   make(map[string]preparedStatementInfo),
		lock:                    &sync.RWMutex{},
	}
}

func (psc *PreparedStatementCache) trackStatementToBePrepared(streamId uint16, forwardDecision forwardDecision) {
	// add the statement info for this query to the transient map of statements to be prepared
	stmtInfo := preparedStatementInfo{forwardDecision}
	psc.lock.Lock()
	psc.statementsBeingPrepared[streamId] = stmtInfo
	psc.lock.Unlock()
}

func (psc *PreparedStatementCache) cachePreparedID(f *Frame) {
	log.Tracef("In cachePreparedID")

	data := f.RawBytes

	kind := int(binary.BigEndian.Uint32(data[9:13]))
	log.Tracef("Kind: %d", kind)
	if kind != 4 {
		// TODO error: this result is not a reply to a PREPARE request
	}

	idLength := int(binary.BigEndian.Uint16(data[13:15]))
	preparedID := string(data[15 : 15+idLength])

	log.Tracef("PreparedID: %s for stream %d", preparedID, f.StreamId)

	psc.lock.Lock()
	log.Tracef("cachePreparedID: lock acquired")
	// move the information about this statement into the cache
	psc.cache[preparedID] = psc.statementsBeingPrepared[f.StreamId]
	log.Tracef("PSInfo set in map for PreparedID: %s", preparedID)
	// remove it from the temporary map
	delete(psc.statementsBeingPrepared, f.StreamId)
	log.Tracef("cachePreparedID: removing statement info from transient map")
	psc.lock.Unlock()
	log.Tracef("cachePreparedID: lock released")

}

func (psc *PreparedStatementCache) retrieveStmtInfoFromCache(preparedID string) (preparedStatementInfo, bool) {
	psc.lock.RLock()
	defer psc.lock.RUnlock()
	stmtInfo, ok := psc.cache[preparedID]
	return stmtInfo, ok
}
