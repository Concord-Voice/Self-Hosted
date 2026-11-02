-- All record and arithmetic validation precedes the first write. Logical time
-- never moves backward, and retention covers that logical time plus ten minutes.
local MAX_SAFE_US = 9007199254740991
local MAX_SAFE_MS = 9007199254740
local MAX_DEBT_US = 120000000
local RETENTION_MS = 600000

local function invalid()
    return redis.error_reply("ERR WS_EVENT_BUDGET_INVALID_STATE")
end

local function unsigned(value)
    if type(value) ~= 'string' or #value == 0 or #value > 16 then
        return nil
    end
    if value ~= '0' and not string.match(value, '^[1-9][0-9]*$') then
        return nil
    end
    -- Compare the bytes before tonumber can round an out-of-range integer.
    if #value == 16 and value > '9007199254740991' then
        return nil
    end
    return tonumber(value)
end

local function ceilMillis(micros)
    local quotient = math.floor(micros / 1000)
    if micros % 1000 ~= 0 then
        return quotient + 1
    end
    return quotient
end

if #KEYS ~= 1 or #ARGV ~= 3 or ARGV[1] ~= '1' then
    return invalid()
end
local interval = unsigned(ARGV[2])
local burst = unsigned(ARGV[3])
if not interval or not burst or interval < 3125 or interval > 20000000
    or burst < 2 or burst > 640 then
    return invalid()
end
if interval > math.floor(MAX_DEBT_US / burst) then
    return invalid()
end

local clock = redis.call('TIME')
if type(clock) ~= 'table' or #clock ~= 2 then
    return invalid()
end
local seconds = unsigned(clock[1])
local micros = unsigned(clock[2])
if not seconds or not micros or micros > 999999
    or seconds > math.floor(MAX_SAFE_US / 1000000) then
    return invalid()
end
local wall = seconds * 1000000
if micros > MAX_SAFE_US - wall then
    return invalid()
end
wall = wall + micros

local key = KEYS[1]
local last = wall
local tat = wall
local keyType = redis.call('TYPE', key).ok
if keyType ~= 'none' then
    if keyType ~= 'hash' or redis.call('HLEN', key) ~= 3 then
        return invalid()
    end
    if redis.call('HGET', key, 'v') ~= '1' then
        return invalid()
    end
    last = unsigned(redis.call('HGET', key, 'last_us'))
    tat = unsigned(redis.call('HGET', key, 'tat_us'))
    if not last or not tat or tat - last > MAX_DEBT_US then
        return invalid()
    end
end

local now = math.max(wall, last)
local allowAt = tat - (burst - 1) * interval
local allowed = 0
local retry = 0
local newTat = tat
if now >= allowAt then
    local base = math.max(now, tat)
    if interval > MAX_SAFE_US - base then
        return invalid()
    end
    newTat = base + interval
    allowed = 1
else
    retry = ceilMillis(allowAt - now)
    if retry < 1 or retry > 120000 then
        return invalid()
    end
end

-- Subtract first: both millisecond operands are exact and their difference
-- is nonnegative. Check the margin and absolute horizon before either sum.
local retained = ceilMillis(now) - math.floor(wall / 1000)
if retained > MAX_SAFE_MS - RETENTION_MS then
    return invalid()
end
local ttl = RETENTION_MS + retained
if ttl <= 0 or ceilMillis(wall) > MAX_SAFE_MS - ttl then
    return invalid()
end

redis.call('HSET', key, 'v', '1',
    'last_us', string.format('%.0f', now),
    'tat_us', string.format('%.0f', newTat))
-- Failure here is uncertain execution: Redis does not roll back the HSET.
redis.call('PEXPIRE', key, ttl)
return {1, allowed, retry}
