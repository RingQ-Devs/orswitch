package core

// RedisGet reads one key of the RingQ backend's Redis ("" when it is missing or Redis is unreachable).
func (c *Core) RedisGet(key string) string { return c.redisStr("GET", key) }
