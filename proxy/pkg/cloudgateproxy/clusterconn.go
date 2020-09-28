package cloudgateproxy

import (
	"fmt"
	"github.com/riptano/cloud-gate/proxy/pkg/metrics"
	log "github.com/sirupsen/logrus"
	"net"
	"sync"
	"time"
)

type ClusterConnectionInfo struct {
	ipAddress			string
	port				int
	isOriginCassandra	bool
	username			string
	password			string
}

type ClusterType string
const (
	ORIGIN_CASSANDRA = ClusterType("originCassandra")
	TARGET_CASSANDRA = ClusterType("targetCassandra")
)

type ClusterConnector struct {
	connection 				net.Conn
	clusterResponseChannels	map[uint16]chan *Frame		// map of channels, keyed on streamID, on which to send the response to a request
	clusterType				ClusterType

	username				string
	password				string
	lock 					*sync.RWMutex		// TODO do we need a lock here?
	metrics					*metrics.MetricsOld
}

func NewClusterConnectionInfo(ipAddress string, port int, isOriginCassandra bool, username string, password string) *ClusterConnectionInfo {
	return &ClusterConnectionInfo{
		ipAddress: ipAddress,
		port: port,
		isOriginCassandra: isOriginCassandra,
		username: username,
		password: password,
	}
}

func NewClusterConnector(	connInfo *ClusterConnectionInfo,
							metrics *metrics.MetricsOld) *ClusterConnector{

	var clusterType ClusterType
	 if connInfo.isOriginCassandra {
	 	clusterType = ORIGIN_CASSANDRA
	 } else {
	 	clusterType = TARGET_CASSANDRA
	 }

	return &ClusterConnector{
		connection: 				establishConnection(connInfo.getConnectionString()),
		clusterResponseChannels:	make(map[uint16]chan *Frame),
		clusterType: 				clusterType,
		username:					connInfo.username,
		password:					connInfo.password,
		lock: 						&sync.RWMutex{},
		metrics:					metrics,
	}
}


func (cc *ClusterConnector) run() {
	cc.runResponseListeningLoop()
}


/**
 *	Starts a long-running loop that listens for replies being sent by the cluster
 */
func (cc *ClusterConnector) runResponseListeningLoop() {

	log.Debugf("Listening to replies sent by node %s", cc.connection.RemoteAddr())
	go func() {
		for {
			frameHeader := make([]byte, cassHdrLen)
			response, _ := parseFrame(cc.connection, frameHeader, cc.metrics)

			log.Debugf(
				"Received response from %s (%s), opcode=%d, streamid=%d: %v",
				cc.clusterType, cc.connection.RemoteAddr(), response.Opcode,
				response.Stream, string(*&response.RawBytes))

			cc.lock.Lock()
			if responseChannel, ok := cc.clusterResponseChannels[response.Stream]; !ok {
				log.Errorf("could not find stream %d in clusterResponseChannels for client %s. Cluster %v", response.Stream, cc.clusterType)
			} else {
				// Note: the boolean response is sent on the channel here - this will unblock the forwardToCluster goroutine waiting on this
				responseChannel <- response
			}
			cc.lock.Unlock()
		}
	}()
}

/**
 *	Submits the request on cluster connection.
 *	Sends the response to the request being handled to the caller (handleRequest) on the channel responseToCallerChan.
 *	Adds a channel to a map (clusterResponseChannels) keyed on streamID. This channel is used by the dequeuer to communicate the response back to this goroutine.
 *	It is this goroutine that has to receive the response, so it can enforce the timeout in case of connection disruption
 */
func (cc *ClusterConnector) forwardToCluster(rawBytes []byte, streamId uint16) chan *Frame{
	responseToCallerChan := make(chan *Frame)

	go func() {
		defer close(responseToCallerChan)

		responseFromClusterChan := cc.createChannelForClusterResponse(streamId)
		// once the response has been sent to the caller, remove the channel from the map as it has served its purpose
		defer cc.deleteChannelForClusterResponse(streamId)

		err := cc.sendRequestToCluster(rawBytes)
		if err != nil {
			log.Errorf("Error while sending request to %s: %s", cc.connection.RemoteAddr(), err)
		}

		timeout := time.NewTimer(queryTimeout)
		for {
			select {
			case response := <-responseFromClusterChan:
				log.Debugf("Received response from %s for query with stream id %d", cc.clusterType, response.Stream)
				responseToCallerChan <- response
				timeout.Stop()
			case <-timeout.C:
				log.Debugf("Timeout for query %d from %s", streamId, cc.clusterType)
				// TODO clean up channel for that stream (already being done via defer deleteChannelForClusterResponse ?)
			}
		}
	}()

	return responseToCallerChan
}

/**
 *	Creates channel on which the dequeuer will send the response to the request with this streamId and adds it to the map
 */
func (cc *ClusterConnector) createChannelForClusterResponse(streamId uint16) chan *Frame {
	cc.lock.Lock()
	defer cc.lock.Unlock()
	cc.clusterResponseChannels[streamId] = make(chan *Frame, 1)

	return cc.clusterResponseChannels[streamId]
}

/**
 *	Removes the response channel for this streamId from the map
 */
func (cc *ClusterConnector) deleteChannelForClusterResponse(streamId uint16) {
	cc.lock.Lock()
	defer cc.lock.Unlock()
	close(cc.clusterResponseChannels[streamId])
	delete(cc.clusterResponseChannels, streamId)
}

func (cc *ClusterConnector) sendRequestToCluster(rawBytes []byte) error {
	// TODO: remove locks (we should only need them for the streamid channel map?)
	cc.lock.Lock()
	defer cc.lock.Unlock()
	log.Debugf("Executing %x on cluster with address %v, len=%d", rawBytes[:9], cc.connection.RemoteAddr(), len(rawBytes))
	err := writeToConnection(cc.connection, rawBytes)
	return err
}

func (cci *ClusterConnectionInfo) getConnectionString() string {
	return fmt.Sprintf("%s:%d", cci.ipAddress, cci.port)
}