package repository

import (
	"github.com/G-Research/k8s-batch/internal/armada/api"
	"github.com/G-Research/k8s-batch/internal/common/util"
	"github.com/go-redis/redis"
	"github.com/gogo/protobuf/proto"
	"github.com/prometheus/common/log"
	"strconv"
	"time"
)

const jobObjectPrefix = "Job:"
const jobQueuePrefix = "Job:Queue:"
const jobLeasedPrefix = "Job:Leased:"
const jobClusterMapKey = "Job:ClusterId"
const jobQueueMapKey = "Job:QueueName"

type JobRepository interface {
	CreateJob(request *api.JobRequest) *api.Job
	AddJob(job *api.Job) error
	PeekQueue(queue string, limit int64) ([]*api.Job, error)
	FilterActiveQueues(queues []*api.Queue) ([]*api.Queue, error)
	TryLeaseJobs(clusterId string, queue string, jobs []*api.Job) ([]*api.Job, error)
	RenewLease(clusterId string, jobIds []string) (renewed []string, e error)
	ExpireLeases(queue string, deadline time.Time) error
	Remove(jobIds []string) (cleanedJobs []string, e error)
}

type RedisJobRepository struct {
	db *redis.Client
}

func NewRedisJobRepository(db *redis.Client) *RedisJobRepository {
	return &RedisJobRepository{db: db}
}

func (repo RedisJobRepository) CreateJob(request *api.JobRequest) *api.Job {
	j := api.Job{
		Id:       util.NewULID(),
		Queue:    request.Queue,
		JobSetId: request.JobSetId,

		Priority: request.Priority,

		PodSpec: request.PodSpec,
		Created: time.Now(),
	}
	return &j
}

func (repo RedisJobRepository) RenewLease(clusterId string, jobIds []string) (renewedJobIds []string, e error) {
	jobs, e := repo.getJobIdentities(jobIds)
	if e != nil {
		return nil, e
	}
	return repo.leaseJobs(clusterId, jobs)
}

func (repo RedisJobRepository) Remove(jobIds []string) (cleanedJobIds []string, e error) {

	jobs, e := repo.getJobIdentities(jobIds)

	pipe := repo.db.Pipeline()
	cmds := make(map[string]*redis.IntCmd)
	for _, job := range jobs {
		cmds[job.id] = repo.db.ZRem(jobLeasedPrefix+job.queueName, job.id)
	}

	_, e = pipe.Exec()
	if e != nil {
		return nil, e
	}

	cleanedJobs := []string{}

	for jobId, cmd := range cmds {
		modified, e := cmd.Result()
		if e == nil && modified > 0 {
			cleanedJobs = append(cleanedJobs, jobId)
		}
	}

	// TODO removing only leases for now, cleanup everything else
	return cleanedJobs, nil
}

func (repo RedisJobRepository) AddJob(job *api.Job) error {
	pipe := repo.db.TxPipeline()

	jobData, e := proto.Marshal(job)
	if e != nil {
		return e
	}

	pipe.ZAdd(jobQueuePrefix+job.Queue, redis.Z{
		Member: job.Id,
		Score:  job.Priority})

	pipe.Set(jobObjectPrefix+job.Id, jobData, 0)
	pipe.HSet(jobQueueMapKey, job.Id, job.Queue)

	_, e = pipe.Exec()
	return e
}

func (repo RedisJobRepository) PeekQueue(queue string, limit int64) ([]*api.Job, error) {
	ids, e := repo.db.ZRange(jobQueuePrefix+queue, 0, limit-1).Result()
	if e != nil {
		return nil, e
	}
	return repo.GetJobsByIds(ids)
}

// returns list of jobs which are successfully leased
func (repo RedisJobRepository) TryLeaseJobs(clusterId string, queue string, jobs []*api.Job) ([]*api.Job, error) {
	jobIds := []jobIdentity{}
	jobById := map[string]*api.Job{}
	for _, job := range jobs {
		jobIds = append(jobIds, jobIdentity{job.Id, queue})
		jobById[job.Id] = job
	}

	leasedIds, e := repo.leaseJobs(clusterId, jobIds)
	if e != nil {
		return nil, e
	}

	leasedJobs := make([]*api.Job, 0)
	for _, id := range leasedIds {
		leasedJobs = append(leasedJobs, jobById[id])
	}
	return leasedJobs, nil
}

func (repo RedisJobRepository) GetJobsByIds(ids []string) ([]*api.Job, error) {
	pipe := repo.db.Pipeline()
	var cmds []*redis.StringCmd
	for _, id := range ids {
		cmds = append(cmds, pipe.Get(jobObjectPrefix+id))
	}
	_, e := pipe.Exec()
	if e != nil {
		return nil, e
	}

	var jobs []*api.Job
	for _, cmd := range cmds {
		d, _ := cmd.Bytes()
		job := &api.Job{}
		e = proto.Unmarshal(d, job)
		if e != nil {
			return nil, e
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (repo RedisJobRepository) FilterActiveQueues(queues []*api.Queue) ([]*api.Queue, error) {
	pipe := repo.db.Pipeline()
	cmds := make(map[*api.Queue]*redis.IntCmd)
	for _, queue := range queues {
		// empty (even sorted) sets gets deleted by redis automatically
		cmds[queue] = pipe.Exists(jobQueuePrefix + queue.Name)
	}
	_, e := pipe.Exec()
	if e != nil {
		return nil, e
	}

	var active []*api.Queue
	for queue, cmd := range cmds {
		if cmd.Val() > 0 {
			active = append(active, queue)
		}
	}
	return active, nil
}

func (repo RedisJobRepository) ExpireLeases(queue string, deadline time.Time) error {
	maxScore := strconv.FormatInt(deadline.UnixNano(), 10)

	ids, e := repo.db.ZRangeByScore(jobLeasedPrefix+queue, redis.ZRangeBy{Max: maxScore, Min: "-Inf"}).Result()
	if e != nil {
		return e
	}
	expiredJobs, e := repo.GetJobsByIds(ids)
	if e != nil {
		return e
	}

	if len(expiredJobs) == 0 {
		return nil
	}

	pipe := repo.db.Pipeline()
	expireScript.Load(pipe)
	for _, job := range expiredJobs {
		expire(pipe, job.Queue, job.Id, job.Created, deadline)
	}
	_, e = pipe.Exec()
	return e
}

type jobIdentity struct {
	id        string
	queueName string
}

func (repo RedisJobRepository) getJobIdentities(jobIds []string) ([]jobIdentity, error) {
	queues, e := repo.db.HMGet(jobQueueMapKey, jobIds...).Result()
	if e != nil {
		return nil, e
	}

	jobIdentities := []jobIdentity{}
	for i, queue := range queues {
		jobIdentities = append(jobIdentities, jobIdentity{jobIds[i], queue.(string)})
	}
	return jobIdentities, nil
}

func (repo RedisJobRepository) leaseJobs(clusterId string, jobs []jobIdentity) ([]string, error) {

	now := time.Now()
	pipe := repo.db.Pipeline()

	leaseJobScript.Load(pipe)

	cmds := make(map[string]*redis.Cmd)
	for _, job := range jobs {
		cmds[job.id] = leaseJob(pipe, job.queueName, clusterId, job.id, now)
	}
	_, e := pipe.Exec()
	if e != nil {
		return nil, e
	}

	leasedJobs := make([]string, 0)
	for jobId, cmd := range cmds {
		value, e := cmd.Int()
		if e != nil {
			log.Error(e)
		} else if value == alreadyAllocatedByDifferentCluster {
			log.With("jobId", jobId).Info("Job Already allocated to different cluster")
		} else {
			leasedJobs = append(leasedJobs, jobId)
		}
	}
	return leasedJobs, nil
}

func leaseJob(db redis.Cmdable, queueName string, clusterId string, jobId string, now time.Time) *redis.Cmd {
	return leaseJobScript.Run(db, []string{jobQueuePrefix + queueName, jobLeasedPrefix + queueName, jobClusterMapKey},
		clusterId, jobId, float64(now.UnixNano()))
}

const alreadyAllocatedByDifferentCluster = -42

var leaseJobScript = redis.NewScript(`
local queue = KEYS[1]
local leasedJobsSet = KEYS[2]
local clusterAssociation = KEYS[3]

local clusterId = ARGV[1]
local jobId = ARGV[2]
local currentTime = ARGV[3]

local exists = redis.call('ZREM', queue, jobId)

if exists == 1 then 
	redis.call('HSET', clusterAssociation, jobId, clusterId)
	return redis.call('ZADD', leasedJobsSet, currentTime, jobId)
else
	local currentClusterId = redis.call('HGET', clusterAssociation, jobId)
	local score = redis.call('ZSCORE', leasedJobsSet, jobId)
	
	if currentClusterId ~= clusterId then
		return -42
	end

	if score == nil then
		return redis.error_reply('Job is missing from leased jobs set.')
	end

	return redis.call('ZADD', leasedJobsSet, currentTime, jobId)
end
`)

func expire(db redis.Cmdable, queueName string, jobId string, created time.Time, deadline time.Time) *redis.Cmd {
	return expireScript.Run(db, []string{jobQueuePrefix + queueName, jobLeasedPrefix + queueName},
		jobId, float64(created.UnixNano()), float64(deadline.UnixNano()))
}

var expireScript = redis.NewScript(`
local queue = KEYS[1]
local leasedJobsSet = KEYS[2]

local jobId = ARGV[1]
local created = tonumber(ARGV[2])
local deadline = tonumber(ARGV[3])

local leasedTime = tonumber(redis.call('ZSCORE', leasedJobsSet, jobId))

if leasedTime ~= nil and leasedTime < deadline then
	local exists = redis.call('ZREM', leasedJobsSet, jobId)
	if exists then
		return redis.call('ZADD', queue, created, jobId)
	else
		return 0
	end
end
`)
