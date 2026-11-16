package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fikohc/memelo/telegram-service/conf"
	"github.com/fikohc/memelo/telegram-service/entity"
	"go.uber.org/fx"
)

const (
	serviceChatQueueSize         = 64
	serviceChatPriorityQueueSize = 32
	serviceChatWorkers           = 2
)

// BackgroundServiceChatUploader queues uploads of meme media to the service
// chat and runs them on background workers through ServiceChatMediaUploader,
// so inline results can send cached media instead of URLs (Telegram fetches
// URLs only up to 5 MB for photos and 20 MB for videos).
type BackgroundServiceChatUploader interface {
	// EnqueueMaybe schedules a background upload. It never blocks; duplicates of
	// an in-flight upload and overflow of the queue are dropped.
	EnqueueMaybe(item entity.MemeSearchResult)

	// EnqueuePriority schedules an upload ahead of everything queued with
	// Enqueue and calls cb with the resulting file_id or error once it is done.
	// An item already waiting in the ordinary queue is moved up. It never
	// blocks; if the upload can't be scheduled, cb is called immediately.
	EnqueuePriority(item entity.MemeSearchResult, cb UploadCallback)
}

// UploadCallback receives the Bot API file_id of an uploaded item, or the
// upload error. It is called from an upload worker and must not block.
type UploadCallback func(fileId string, err error)

var (
	errServiceChatDisabled  = errors.New("service chat upload is disabled")
	errServiceChatQueueFull = errors.New("service chat priority queue is full")
)

// uploadJob is one pending upload. A job can sit in both queues when it is
// moved up to the priority queue; started makes sure it runs only once.
type uploadJob struct {
	item       entity.MemeSearchResult
	callbacks  []UploadCallback
	started    bool
	enqueuedAt time.Time
}

const (
	queueOrdinary = "ordinary"
	queuePriority = "priority"
)

type BackgroundServiceChatUploaderImpl struct {
	uploader ServiceChatMediaUploader
	chatId   int64
	log      *slog.Logger

	queue         chan *uploadJob
	priorityQueue chan *uploadJob

	jobsMu sync.Mutex
	jobs   map[string]*uploadJob
}

func (u *BackgroundServiceChatUploaderImpl) EnqueueMaybe(item entity.MemeSearchResult) {
	if u.chatId == 0 {
		return
	}

	u.jobsMu.Lock()
	defer u.jobsMu.Unlock()
	if _, exists := u.jobs[item.Id]; exists {
		return
	}
	job := &uploadJob{item: item, enqueuedAt: time.Now()}
	select {
	case u.queue <- job:
		u.jobs[item.Id] = job
	default:
		u.log.Warn("Service chat upload queue is full, dropping", "memeId", item.Id)
	}
}

func (u *BackgroundServiceChatUploaderImpl) EnqueuePriority(item entity.MemeSearchResult, cb UploadCallback) {
	if u.chatId == 0 {
		cb("", errServiceChatDisabled)
		return
	}

	u.jobsMu.Lock()
	job, exists := u.jobs[item.Id]
	if exists {
		job.callbacks = append(job.callbacks, cb)
		if job.started {
			u.jobsMu.Unlock()
			return
		}
	} else {
		job = &uploadJob{item: item, callbacks: []UploadCallback{cb}, enqueuedAt: time.Now()}
	}

	select {
	case u.priorityQueue <- job:
		u.jobs[item.Id] = job
		u.jobsMu.Unlock()
	default:
		if exists {
			// Still waiting in the ordinary queue; cb runs when it gets there.
			u.jobsMu.Unlock()
			u.log.Warn("Service chat priority queue is full, keeping ordinary position", "memeId", item.Id)
			return
		}
		u.jobsMu.Unlock()
		u.log.Warn("Service chat priority queue is full, dropping", "memeId", item.Id)
		cb("", errServiceChatQueueFull)
	}
}

func (u *BackgroundServiceChatUploaderImpl) worker(ctx context.Context) {
	for {
		job, queue, ok := u.nextJob(ctx)
		if !ok {
			return
		}
		u.run(ctx, job, queue)
	}
}

// nextJob takes a priority job if there is one, otherwise waits for either
// queue. It also returns the name of the queue the job came from.
func (u *BackgroundServiceChatUploaderImpl) nextJob(ctx context.Context) (*uploadJob, string, bool) {
	select {
	case job := <-u.priorityQueue:
		return job, queuePriority, true
	default:
	}
	select {
	case <-ctx.Done():
		return nil, "", false
	case job := <-u.priorityQueue:
		return job, queuePriority, true
	case job := <-u.queue:
		return job, queueOrdinary, true
	}
}

func (u *BackgroundServiceChatUploaderImpl) run(ctx context.Context, job *uploadJob, queue string) {
	u.jobsMu.Lock()
	if job.started {
		// Moved up to the priority queue and already taken from there.
		u.jobsMu.Unlock()
		u.log.DebugContext(ctx, "Service chat upload skipped, already taken from priority queue",
			"memeId", job.item.Id,
			"queue", queue)
		return
	}
	job.started = true
	waitingCallbacks := len(job.callbacks)
	u.jobsMu.Unlock()

	u.log.InfoContext(ctx, "Service chat upload picked up",
		"memeId", job.item.Id,
		"type", job.item.Type,
		"queue", queue,
		"queuedFor", time.Since(job.enqueuedAt),
		"waitingCallbacks", waitingCallbacks,
		"priorityQueueLen", len(u.priorityQueue),
		"ordinaryQueueLen", len(u.queue))

	started := time.Now()
	fileId, err := u.uploader.Upload(ctx, job.item)

	u.jobsMu.Lock()
	delete(u.jobs, job.item.Id)
	callbacks := job.callbacks
	u.jobsMu.Unlock()

	if err != nil {
		u.log.ErrorContext(ctx, "Service chat upload failed",
			"memeId", job.item.Id,
			"queue", queue,
			"duration", time.Since(started),
			"callbacks", len(callbacks),
			"error", err)
	} else {
		u.log.InfoContext(ctx, "Service chat upload finished",
			"memeId", job.item.Id,
			"type", job.item.Type,
			"queue", queue,
			"duration", time.Since(started),
			"callbacks", len(callbacks))
	}

	for _, cb := range callbacks {
		cb(fileId, err)
	}
}

func NewBackgroundServiceChatUploader(
	lc fx.Lifecycle,
	uploader ServiceChatMediaUploader,
	cfg *conf.Config,
) BackgroundServiceChatUploader {
	u := &BackgroundServiceChatUploaderImpl{
		uploader: uploader,
		chatId:   cfg.Telegram.ServiceChatId,
		log:      slog.With("service", "BackgroundServiceChatUploader"),

		queue:         make(chan *uploadJob, serviceChatQueueSize),
		priorityQueue: make(chan *uploadJob, serviceChatPriorityQueueSize),
		jobs:          make(map[string]*uploadJob),
	}
	if u.chatId == 0 {
		u.log.Warn("telegram.ServiceChatId is not set, media won't be cached in the service chat")
	}

	var cancel context.CancelFunc
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			var ctx context.Context
			ctx, cancel = context.WithCancel(context.Background())
			for range serviceChatWorkers {
				go u.worker(ctx)
			}
			return nil
		},
		OnStop: func(context.Context) error {
			if cancel != nil {
				cancel()
			}
			return nil
		},
	})
	return u
}
