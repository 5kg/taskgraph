package meritop

import (
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/coreos/go-etcd/etcd"
)

type taskRole int

const (
	noneRole taskRole = iota
	parentRole
	childRole
)

const (
	DataRequestPrefix string = "/datareq"
	DataRequestTaskID string = "taskID"
	DataRequestReq    string = "req"
)

// This interface is used by application during taskgraph configuration phase.
type Bootstrap interface {
	// These allow application developer to set the task configuration so framework
	// implementation knows which task to invoke at each node.
	SetTaskBuilder(taskBuilder TaskBuilder)

	// This allow the application to specify how tasks are connection at each epoch
	SetTopology(topology Topology)

	// After all the configure is done, driver need to call start so that all
	// nodes will get into the event loop to run the application.
	Start()
}

// Note that framework can decide how update can be done, and how to serve the updatelog.
type BackedUpFramework interface {
	// Ask framework to do update on this update on this task, which consists
	// of one primary and some backup copies.
	Update(taskID uint64, log UpdateLog)
}

type Framework interface {
	// These two are useful for task to inform the framework their status change.
	// metaData has to be really small, since it might be stored in etcd.
	// Flags that parent/child's metadata of the current task is ready.
	FlagParentMetaReady(meta string)
	FlagChildMetaReady(meta string)

	// This allow the task implementation query its neighbors.
	GetTopology() Topology

	// Some task can inform all participating tasks to exit.
	Exit()

	// Some task can inform all participating tasks to new epoch
	SetEpoch(epoch uint64)

	GetLogger() log.Logger

	// Request data from parent or children.
	DataRequest(toID uint64, meta string)

	// This is used to figure out taskid for current node
	GetTaskID() uint64
}

type framework struct {
	// These should be passed by outside world
	name     string
	etcdURLs []string

	// user defined interfaces
	task     Task
	topology Topology

	taskID       uint64
	epoch        uint64
	etcdClient   *etcd.Client
	stops        []chan bool
	ln           net.Listener
	addressMap   map[uint64]string // taskId -> node address. Maybe in etcd later.
	dataRespChan chan *dataResponse
}

type dataResponse struct {
	taskID uint64
	req    string
	data   []byte
}

func (f *framework) parentOrChild(taskID uint64) taskRole {
	for _, id := range f.topology.GetParents(f.epoch) {
		if taskID == id {
			return parentRole
		}
	}

	for _, id := range f.topology.GetChildren(f.epoch) {
		if taskID == id {
			return childRole
		}
	}
	return noneRole
}

func (f *framework) start() {
	f.etcdClient = etcd.NewClient(f.etcdURLs)
	f.topology.SetTaskID(f.taskID)
	f.epoch = 0
	f.stops = make([]chan bool, 0)
	f.dataRespChan = make(chan *dataResponse, 100)

	// setup etcd watches
	// - create self's parent and child meta flag
	// - watch parents' child meta flag
	// - watch children's parent meta flag
	f.etcdClient.Create(MakeParentMetaPath(f.name, f.GetTaskID()), "", 0)
	f.etcdClient.Create(MakeChildMetaPath(f.name, f.GetTaskID()), "", 0)
	parentStops := f.watchAll(parentRole, f.topology.GetParents(f.epoch))
	childStops := f.watchAll(childRole, f.topology.GetChildren(f.epoch))
	f.stops = append(f.stops, parentStops...)
	f.stops = append(f.stops, childStops...)

	go f.startHttpServerForDataRequest()
	go f.dataResponseEventLoop()

	// After framework init finished, it should init task.
	f.task.SetEpoch(f.epoch)
	f.task.Init(f.taskID, f, nil)
}

func newDataReqHandler(f *framework) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(DataRequestPrefix, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		fromIDStr := q.Get(DataRequestTaskID)
		fromID, err := strconv.ParseUint(fromIDStr, 0, 64)
		if err != nil {
			log.Fatalf("taskID in query couldn't be parsed: %s", fromIDStr)
		}
		req := q.Get(DataRequestReq)
		var serveData func(uint64, string) []byte
		switch f.parentOrChild(fromID) {
		case parentRole:
			serveData = f.task.ServeAsChild
		case childRole:
			serveData = f.task.ServeAsParent
		default:
			panic("unimplemented")
		}
		d := serveData(fromID, req)

		if _, err := w.Write(d); err != nil {
			log.Printf("response write errored: %v", err)
		}
	})
	return mux
}

// Framework http server for data request.
// Each request will be in the format: "/datareq/{taskID}/{req}".
// "taskID" indicates the requesting task. "req" is the meta data for this request.
// On success, it should respond with requested data in http body.
func (f *framework) startHttpServerForDataRequest() {
	log.Printf("framework: serving http data request on %s", f.ln.Addr())
	if err := http.Serve(f.ln, newDataReqHandler(f)); err != nil {
		log.Fatalf("http.Serve() returns error: %v\n", err)
	}
}

// Framework event loop handles data response for requests sent in DataRequest().
func (f *framework) dataResponseEventLoop() {
	for {
		dataResp := <-f.dataRespChan

		var dataReady func(uint64, string, []byte)
		switch f.parentOrChild(dataResp.taskID) {
		case parentRole:
			dataReady = f.task.ParentDataReady
		case childRole:
			dataReady = f.task.ChildDataReady
		default:
			panic("unimplemented")
		}

		go dataReady(dataResp.taskID, dataResp.req, dataResp.data)
	}
}

func (f *framework) stop() {
	for _, c := range f.stops {
		close(c)
	}
}

func (f *framework) FlagParentMetaReady(meta string) {
	f.etcdClient.Set(
		MakeParentMetaPath(f.name, f.GetTaskID()),
		meta,
		0)
}

func (f *framework) FlagChildMetaReady(meta string) {
	f.etcdClient.Set(
		MakeChildMetaPath(f.name, f.GetTaskID()),
		meta,
		0)
}

func (f *framework) SetEpoch(epoch uint64) {
	f.epoch = epoch
}

func (f *framework) watchAll(who taskRole, taskIDs []uint64) []chan bool {
	stops := make([]chan bool, len(taskIDs))

	for i, taskID := range taskIDs {
		receiver := make(chan *etcd.Response, 10)
		stop := make(chan bool, 1)
		stops[i] = stop

		var watchPath string
		var taskCallback func(uint64, string)
		switch who {
		case parentRole:
			// Watch parent's child.
			watchPath = MakeChildMetaPath(f.name, taskID)
			taskCallback = f.task.ParentMetaReady
		case childRole:
			// Watch child's parent.
			watchPath = MakeParentMetaPath(f.name, taskID)
			taskCallback = f.task.ChildMetaReady
		default:
			panic("unimplemented")
		}

		go f.etcdClient.Watch(watchPath, 0, false, receiver, stop)
		go func(receiver <-chan *etcd.Response, taskID uint64) {
			for {
				resp, ok := <-receiver
				if !ok {
					return
				}
				if resp.Action != "set" {
					continue
				}
				taskCallback(taskID, resp.Node.Value)
			}
		}(receiver, taskID)
	}
	return stops
}

func (f *framework) DataRequest(toID uint64, req string) {
	// getAddressFromTaskID
	addr, ok := f.addressMap[toID]
	if !ok {
		log.Fatalf("ID = %d not found", toID)
		return
	}
	u := url.URL{
		Scheme: "http",
		Host:   addr,
		Path:   DataRequestPrefix,
	}
	q := u.Query()
	q.Add(DataRequestTaskID, strconv.FormatUint(f.taskID, 10))
	q.Add(DataRequestReq, req)
	u.RawQuery = q.Encode()
	urlStr := u.String()
	// send request
	// pass the response to the awaiting event loop for data response
	go func(urlStr string) {
		resp, err := http.Get(urlStr)
		if err != nil {
			log.Fatalf("http.Get(%s) returns error: %v", urlStr, err)
		}
		if resp.StatusCode != 200 {
			log.Fatalf("response code = %d, assume = %d", resp.StatusCode, 200)
		}
		data, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Fatalf("ioutil.ReadAll(%v) returns error: %v", resp.Body, err)
		}
		dataResp := &dataResponse{
			taskID: toID,
			req:    req,
			data:   data,
		}
		f.dataRespChan <- dataResp
	}(urlStr)
}

func (f *framework) GetTopology() Topology {
	panic("unimplemented")
}

func (f *framework) Exit() {
}

func (f *framework) GetLogger() log.Logger {
	panic("unimplemented")
}

func (f *framework) GetTaskID() uint64 {
	return f.taskID
}
