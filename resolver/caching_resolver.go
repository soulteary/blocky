package resolver

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/hako/durafmt"

	"github.com/0xERR0R/blocky/cache/expirationcache"
	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/evt"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/redis"
	"github.com/0xERR0R/blocky/util"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

const defaultCachingCleanUpInterval = 5 * time.Second

// CachingResolver caches answers from dns queries with their TTL time,
// to avoid external resolver calls for recurrent queries
type CachingResolver struct {
	NextResolver
	minCacheTimeSec, maxCacheTimeSec int
	cacheTimeNegative                time.Duration
	resultCache                      expirationcache.ExpiringCache
	prefetchExpires                  time.Duration
	prefetchThreshold                int
	prefetchingNameCache             expirationcache.ExpiringCache
	redisClient                      *redis.Client
	redisEnabled                     bool
}

// cacheValue includes query answer and prefetch flag
type cacheValue struct {
	answer   []dns.RR
	prefetch bool
}

// NewCachingResolver creates a new resolver instance
func NewCachingResolver(cfg config.CachingConfig, redis *redis.Client) *CachingResolver {
	c := &CachingResolver{
		minCacheTimeSec:   int(time.Duration(cfg.MinCachingTime).Seconds()),
		maxCacheTimeSec:   int(time.Duration(cfg.MaxCachingTime).Seconds()),
		cacheTimeNegative: time.Duration(cfg.CacheTimeNegative),
		redisClient:       redis,
		redisEnabled:      (redis != nil),
	}

	configureCaches(c, &cfg)

	if c.redisEnabled {
		setupRedisCacheSubscriber(c)
		c.redisClient.GetRedisCache()
	}

	return c
}

func configureCaches(c *CachingResolver, cfg *config.CachingConfig) {
	cleanupOption := expirationcache.WithCleanUpInterval(defaultCachingCleanUpInterval)
	maxSizeOption := expirationcache.WithMaxSize(uint(cfg.MaxItemsCount))

	if cfg.Prefetching {
		c.prefetchExpires = time.Duration(cfg.PrefetchExpires)

		c.prefetchThreshold = cfg.PrefetchThreshold

		c.prefetchingNameCache = expirationcache.NewCache(
			expirationcache.WithCleanUpInterval(time.Minute),
			expirationcache.WithMaxSize(uint(cfg.PrefetchMaxItemsCount)),
		)

		c.resultCache = expirationcache.NewCache(
			cleanupOption,
			maxSizeOption,
			expirationcache.WithOnExpiredFn(c.onExpired),
		)
	} else {
		c.resultCache = expirationcache.NewCache(cleanupOption, maxSizeOption)
	}
}

func setupRedisCacheSubscriber(c *CachingResolver) {
	logger := log.PrefixedLog("caching_resolver")

	go func() {
		for rc := range c.redisClient.CacheChannel {
			if rc != nil {
				logger.Debug("Received key from redis: ", rc.Key)
				c.putInCache(rc.Key, rc.Response, false, false)
			}
		}
	}()
}

// check if domain was queried > threshold in the time window
func (r *CachingResolver) shouldPrefetch(cacheKey string) bool {
	if r.prefetchThreshold == 0 {
		return true
	}

	cnt, _ := r.prefetchingNameCache.Get(cacheKey)

	return cnt != nil && cnt.(int) > r.prefetchThreshold
}

func (r *CachingResolver) onExpired(cacheKey string) (val interface{}, ttl time.Duration) {
	qType, domainName := util.ExtractCacheKey(cacheKey)

	logger := log.PrefixedLog("caching_resolver")

	if r.shouldPrefetch(cacheKey) {
		logger.Debugf("prefetching '%s' (%s)", util.Obfuscate(domainName), qType.String())

		req := newRequest(fmt.Sprintf("%s.", domainName), qType, logger)
		response, err := r.next.Resolve(req)

		if err == nil {
			if response.Res.Rcode == dns.RcodeSuccess {
				evt.Bus().Publish(evt.CachingDomainPrefetched, domainName)

				return cacheValue{response.Res.Answer, true}, r.adjustTTLs(response.Res.Answer)
			}
		} else {
			util.LogOnError(fmt.Sprintf("can't prefetch '%s' ", domainName), err)
		}
	}

	return nil, 0
}

// Configuration returns a current resolver configuration
func (r *CachingResolver) Configuration() (result []string) {
	if r.maxCacheTimeSec < 0 {
		return configDisabled
	}

	result = append(result, fmt.Sprintf("minCacheTimeInSec = %d", r.minCacheTimeSec))

	result = append(result, fmt.Sprintf("maxCacheTimeSec = %d", r.maxCacheTimeSec))

	result = append(result, fmt.Sprintf("cacheTimeNegative = %s", durafmt.Parse(r.cacheTimeNegative)))

	result = append(result, fmt.Sprintf("prefetching = %t", r.prefetchingNameCache != nil))

	if r.prefetchingNameCache != nil {
		result = append(result, fmt.Sprintf("prefetchExpires = %s", durafmt.Parse(r.prefetchExpires)))

		result = append(result, fmt.Sprintf("prefetchThreshold = %d", r.prefetchThreshold))
	}

	result = append(result, fmt.Sprintf("cache items count = %d", r.resultCache.TotalCount()))

	return
}

// Resolve checks if the current query result is already in the cache and returns it
// or delegates to the next resolver
func (r *CachingResolver) Resolve(request *model.Request) (response *model.Response, err error) {
	logger := log.WithPrefix(request.Log, "caching_resolver")

	if r.maxCacheTimeSec < 0 {
		logger.Debug("skip cache")

		return r.next.Resolve(request)
	}

	resp := new(dns.Msg)
	resp.SetReply(request.Req)

	for _, question := range request.Req.Question {
		domain := util.ExtractDomain(question)
		cacheKey := util.GenerateCacheKey(dns.Type(question.Qtype), domain)
		logger := logger.WithField("domain", util.Obfuscate(domain))

		r.trackQueryDomainNameCount(domain, cacheKey, logger)

		val, ttl := r.resultCache.Get(cacheKey)

		if val != nil {
			logger.Debug("domain is cached")

			evt.Bus().Publish(evt.CachingResultCacheHit, domain)

			v, ok := val.(cacheValue)
			if ok {
				if v.prefetch {
					// Hit from prefetch cache
					evt.Bus().Publish(evt.CachingPrefetchCacheHit, domain)
				}

				// Answer from successful request
				for _, rr := range v.answer {
					// make copy here since entries in cache can be modified by other goroutines (e.g. redis cache)
					cp := dns.Copy(rr)
					cp.Header().Ttl = uint32(ttl.Seconds())

					resp.Answer = append(resp.Answer, cp)
				}

				return &model.Response{Res: resp, RType: model.ResponseTypeCACHED, Reason: "CACHED"}, nil
			}
			// Answer with response code != OK
			resp.Rcode = val.(int)

			return &model.Response{Res: resp, RType: model.ResponseTypeCACHED, Reason: "CACHED NEGATIVE"}, nil
		}

		evt.Bus().Publish(evt.CachingResultCacheMiss, domain)

		logger.WithField("next_resolver", Name(r.next)).Debug("not in cache: go to next resolver")
		response, err = r.next.Resolve(request)

		if err == nil {
			r.putInCache(cacheKey, response, false, r.redisEnabled)
		}
	}

	return response, err
}

func (r *CachingResolver) trackQueryDomainNameCount(domain, cacheKey string, logger *logrus.Entry) {
	if r.prefetchingNameCache != nil {
		var domainCount int
		if x, _ := r.prefetchingNameCache.Get(cacheKey); x != nil {
			domainCount = x.(int)
		}
		domainCount++
		r.prefetchingNameCache.Put(cacheKey, domainCount, r.prefetchExpires)
		totalCount := r.prefetchingNameCache.TotalCount()

		logger.Debugf("domain '%s' was requested %d times, "+
			"total cache size: %d", util.Obfuscate(domain), domainCount, totalCount)
		evt.Bus().Publish(evt.CachingDomainsToPrefetchCountChanged, totalCount)
	}
}

func (r *CachingResolver) putInCache(cacheKey string, response *model.Response, prefetch, publish bool) {
	answer := response.Res.Answer

	if response.Res.Rcode == dns.RcodeSuccess {
		// put value into cache
		r.resultCache.Put(cacheKey, cacheValue{answer, prefetch}, r.adjustTTLs(answer))
	} else if response.Res.Rcode == dns.RcodeNameError {
		if r.cacheTimeNegative > 0 {
			// put return code if NXDOMAIN
			r.resultCache.Put(cacheKey, response.Res.Rcode, r.cacheTimeNegative)
		}
	}

	evt.Bus().Publish(evt.CachingResultCacheChanged, r.resultCache.TotalCount())

	if publish && r.redisClient != nil {
		res := *response.Res
		res.Answer = answer
		r.redisClient.PublishCache(cacheKey, &res)
	}
}

// adjustTTLs calculates and returns the max TTL (considers also the min and max cache time)
// for all records from answer or a negative cache time for empty answer
// adjust the TTL in the answer header accordingly
func (r *CachingResolver) adjustTTLs(answer []dns.RR) (maxTTL time.Duration) {
	var max uint32

	if len(answer) == 0 {
		return r.cacheTimeNegative
	}

	for _, a := range answer {
		// if TTL < mitTTL -> adjust the value, set minTTL
		if r.minCacheTimeSec > 0 {
			if atomic.LoadUint32(&a.Header().Ttl) < uint32(r.minCacheTimeSec) {
				atomic.StoreUint32(&a.Header().Ttl, uint32(r.minCacheTimeSec))
			}
		}

		if r.maxCacheTimeSec > 0 {
			if atomic.LoadUint32(&a.Header().Ttl) > uint32(r.maxCacheTimeSec) {
				atomic.StoreUint32(&a.Header().Ttl, uint32(r.maxCacheTimeSec))
			}
		}

		headerTTL := atomic.LoadUint32(&a.Header().Ttl)
		if max < headerTTL {
			max = headerTTL
		}
	}

	return time.Duration(max) * time.Second
}
