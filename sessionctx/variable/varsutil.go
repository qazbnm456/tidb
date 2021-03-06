// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package variable

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/types"
)

// SetDDLReorgWorkerCounter sets ddlReorgWorkerCounter count.
// Max worker count is maxDDLReorgWorkerCount.
func SetDDLReorgWorkerCounter(cnt int32) {
	if cnt > maxDDLReorgWorkerCount {
		cnt = maxDDLReorgWorkerCount
	}
	atomic.StoreInt32(&ddlReorgWorkerCounter, cnt)
}

// GetDDLReorgWorkerCounter gets ddlReorgWorkerCounter.
func GetDDLReorgWorkerCounter() int32 {
	return atomic.LoadInt32(&ddlReorgWorkerCounter)
}

// GetSessionSystemVar gets a system variable.
// If it is a session only variable, use the default value defined in code.
// Returns error if there is no such variable.
func GetSessionSystemVar(s *SessionVars, key string) (string, error) {
	key = strings.ToLower(key)
	gVal, ok, err := GetSessionOnlySysVars(s, key)
	if err != nil || ok {
		return gVal, errors.Trace(err)
	}
	gVal, err = s.GlobalVarsAccessor.GetGlobalSysVar(key)
	if err != nil {
		return "", errors.Trace(err)
	}
	s.systems[key] = gVal
	return gVal, nil
}

// GetSessionOnlySysVars get the default value defined in code for session only variable.
// The return bool value indicates whether it's a session only variable.
func GetSessionOnlySysVars(s *SessionVars, key string) (string, bool, error) {
	sysVar := SysVars[key]
	if sysVar == nil {
		return "", false, UnknownSystemVar.GenByArgs(key)
	}
	// For virtual system variables:
	switch sysVar.Name {
	case TiDBCurrentTS:
		return fmt.Sprintf("%d", s.TxnCtx.StartTS), true, nil
	case TiDBGeneralLog:
		return fmt.Sprintf("%d", atomic.LoadUint32(&ProcessGeneralLog)), true, nil
	case TiDBConfig:
		conf := config.GetGlobalConfig()
		j, err := json.MarshalIndent(conf, "", "\t")
		if err != nil {
			return "", false, errors.Trace(err)
		}
		return string(j), true, nil
	}
	sVal, ok := s.systems[key]
	if ok {
		return sVal, true, nil
	}
	if sysVar.Scope&ScopeGlobal == 0 {
		// None-Global variable can use pre-defined default value.
		return sysVar.Value, true, nil
	}
	return "", false, nil
}

// GetGlobalSystemVar gets a global system variable.
func GetGlobalSystemVar(s *SessionVars, key string) (string, error) {
	key = strings.ToLower(key)
	gVal, ok, err := GetScopeNoneSystemVar(key)
	if err != nil || ok {
		return gVal, errors.Trace(err)
	}
	gVal, err = s.GlobalVarsAccessor.GetGlobalSysVar(key)
	if err != nil {
		return "", errors.Trace(err)
	}
	return gVal, nil
}

// GetScopeNoneSystemVar checks the validation of `key`,
// and return the default value if its scope is `ScopeNone`.
func GetScopeNoneSystemVar(key string) (string, bool, error) {
	sysVar := SysVars[key]
	if sysVar == nil {
		return "", false, UnknownSystemVar.GenByArgs(key)
	}
	if sysVar.Scope == ScopeNone {
		return sysVar.Value, true, nil
	}
	return "", false, nil
}

// epochShiftBits is used to reserve logical part of the timestamp.
const epochShiftBits = 18

// SetSessionSystemVar sets system variable and updates SessionVars states.
func SetSessionSystemVar(vars *SessionVars, name string, value types.Datum) error {
	name = strings.ToLower(name)
	sysVar := SysVars[name]
	if sysVar == nil {
		return UnknownSystemVar
	}
	if value.IsNull() {
		return vars.deleteSystemVar(name)
	}
	var sVal string
	var err error
	sVal, err = value.ToString()
	if err != nil {
		return errors.Trace(err)
	}
	sVal, err = ValidateSetSystemVar(vars, name, sVal)
	if err != nil {
		return errors.Trace(err)
	}
	return vars.SetSystemVar(name, sVal)
}

// ValidateGetSystemVar checks if system variable exists and validates its scope when get system variable.
func ValidateGetSystemVar(name string, isGlobal bool) error {
	sysVar, exists := SysVars[name]
	if !exists {
		return UnknownSystemVar.GenByArgs(name)
	}
	switch sysVar.Scope {
	case ScopeGlobal, ScopeNone:
		if !isGlobal {
			return ErrIncorrectScope.GenByArgs(name, "GLOBAL")
		}
	case ScopeSession:
		if isGlobal {
			return ErrIncorrectScope.GenByArgs(name, "SESSION")
		}
	}
	return nil
}

// ValidateSetSystemVar checks if system variable satisfies specific restriction.
func ValidateSetSystemVar(vars *SessionVars, name string, value string) (string, error) {
	if strings.EqualFold(value, "DEFAULT") {
		if val := GetSysVar(name); val != nil {
			return val.Value, nil
		}
		return value, UnknownSystemVar.GenByArgs(name)
	}
	switch name {
	case DefaultWeekFormat:
		val, err := strconv.Atoi(value)
		if err != nil {
			return value, ErrWrongTypeForVar.GenByArgs(name)
		}
		if val < 0 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "0", nil
		}
		if val > 7 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "7", nil
		}
	case DelayKeyWrite:
		if strings.EqualFold(value, "ON") || value == "1" {
			return "ON", nil
		} else if strings.EqualFold(value, "OFF") || value == "0" {
			return "OFF", nil
		} else if strings.EqualFold(value, "ALL") || value == "2" {
			return "ALL", nil
		}
		return value, ErrWrongValueForVar.GenByArgs(name, value)
	case FlushTime:
		val, err := strconv.Atoi(value)
		if err != nil {
			return value, ErrWrongTypeForVar.GenByArgs(name)
		}
		if val < 0 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "0", nil
		}
	case GroupConcatMaxLen:
		val, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return value, ErrWrongTypeForVar.GenByArgs(name)
		}
		if val < 4 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "4", nil
		}
		if val > 18446744073709551615 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "18446744073709551615", nil
		}
	case InteractiveTimeout:
		val, err := strconv.Atoi(value)
		if err != nil {
			return value, ErrWrongTypeForVar.GenByArgs(name)
		}
		if val < 1 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "1", nil
		}
	case MaxConnections:
		val, err := strconv.Atoi(value)
		if err != nil {
			return value, ErrWrongTypeForVar.GenByArgs(name)
		}
		if val < 1 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "1", nil
		}
		if val > 100000 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "100000", nil
		}
	case MaxSortLength:
		val, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return value, ErrWrongTypeForVar.GenByArgs(name)
		}
		if val < 4 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "4", nil
		}
		if val > 8388608 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "8388608", nil
		}
	case MaxSpRecursionDepth:
		val, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return value, ErrWrongTypeForVar.GenByArgs(name)
		}
		if val < 0 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "0", nil
		}
		if val > 255 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "255", nil
		}
	case OldPasswords:
		val, err := strconv.Atoi(value)
		if err != nil {
			return value, ErrWrongTypeForVar.GenByArgs(name)
		}
		if val < 0 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "0", nil
		}
		if val > 2 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "2", nil
		}
	case MaxUserConnections:
		val, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return value, ErrWrongTypeForVar.GenByArgs(name)
		}
		if val < 0 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "0", nil
		}
		if val > 4294967295 {
			vars.StmtCtx.AppendWarning(ErrTruncatedWrongValue.GenByArgs(name, value))
			return "4294967295", nil
		}
	case SessionTrackGtids:
		if strings.EqualFold(value, "OFF") || value == "0" {
			return "OFF", nil
		} else if strings.EqualFold(value, "OWN_GTID") || value == "1" {
			return "OWN_GTID", nil
		} else if strings.EqualFold(value, "ALL_GTIDS") || value == "2" {
			return "ALL_GTIDS", nil
		}
		return value, ErrWrongValueForVar.GenByArgs(name, value)
	case TimeZone:
		if strings.EqualFold(value, "SYSTEM") {
			return "SYSTEM", nil
		}
		return value, nil
	case WarningCount, ErrorCount:
		return value, ErrReadOnly.GenByArgs(name)
	case GeneralLog, AvoidTemporalUpgrade, BigTables, CheckProxyUsers, CoreFile, EndMakersInJSON, SQLLogBin, OfflineMode,
		PseudoSlaveMode, LowPriorityUpdates, SkipNameResolve, ForeignKeyChecks, SQLSafeUpdates:
		if strings.EqualFold(value, "ON") || value == "1" {
			return "1", nil
		} else if strings.EqualFold(value, "OFF") || value == "0" {
			return "0", nil
		}
		return value, ErrWrongValueForVar.GenByArgs(name, value)
	case AutocommitVar, TiDBImportingData, TiDBSkipUTF8Check, TiDBOptAggPushDown,
		TiDBOptInSubqUnFolding, TiDBEnableTablePartition,
		TiDBBatchInsert, TiDBDisableTxnAutoRetry, TiDBEnableStreaming,
		TiDBBatchDelete:
		if strings.EqualFold(value, "ON") || value == "1" || strings.EqualFold(value, "OFF") || value == "0" {
			return value, nil
		}
		return value, ErrWrongValueForVar.GenByArgs(name, value)
	case TiDBIndexLookupConcurrency, TiDBIndexLookupJoinConcurrency, TiDBIndexJoinBatchSize,
		TiDBIndexLookupSize,
		TiDBHashJoinConcurrency,
		TiDBHashAggPartialConcurrency,
		TiDBHashAggFinalConcurrency,
		TiDBDistSQLScanConcurrency,
		TiDBIndexSerialScanConcurrency, TiDBDDLReorgWorkerCount,
		TiDBBackoffLockFast, TiDBMaxChunkSize,
		TiDBDMLBatchSize, TiDBOptimizerSelectivityLevel,
		TiDBGeneralLog:
		v, err := strconv.Atoi(value)
		if err != nil {
			return value, ErrWrongTypeForVar.GenByArgs(name)
		}
		if v <= 0 {
			return value, ErrWrongValueForVar.GenByArgs(name, value)
		}
		return value, nil
	case TiDBProjectionConcurrency,
		TIDBMemQuotaQuery,
		TIDBMemQuotaHashJoin,
		TIDBMemQuotaMergeJoin,
		TIDBMemQuotaSort,
		TIDBMemQuotaTopn,
		TIDBMemQuotaIndexLookupReader,
		TIDBMemQuotaIndexLookupJoin,
		TIDBMemQuotaNestedLoopApply,
		TiDBRetryLimit:
		_, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return value, ErrWrongValueForVar.GenByArgs(name)
		}
		return value, nil
	}
	return value, nil
}

// TiDBOptOn could be used for all tidb session variable options, we use "ON"/1 to turn on those options.
func TiDBOptOn(opt string) bool {
	return strings.EqualFold(opt, "ON") || opt == "1"
}

func tidbOptPositiveInt32(opt string, defaultVal int) int {
	val, err := strconv.Atoi(opt)
	if err != nil || val <= 0 {
		return defaultVal
	}
	return val
}

func tidbOptInt64(opt string, defaultVal int64) int64 {
	val, err := strconv.ParseInt(opt, 10, 64)
	if err != nil {
		return defaultVal
	}
	return val
}

func parseTimeZone(s string) (*time.Location, error) {
	if strings.EqualFold(s, "SYSTEM") {
		// TODO: Support global time_zone variable, it should be set to global time_zone value.
		return time.Local, nil
	}

	loc, err := time.LoadLocation(s)
	if err == nil {
		return loc, nil
	}

	// The value can be given as a string indicating an offset from UTC, such as '+10:00' or '-6:00'.
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		d, err := types.ParseDuration(s[1:], 0)
		if err == nil {
			ofst := int(d.Duration / time.Second)
			if s[0] == '-' {
				ofst = -ofst
			}
			return time.FixedZone("UTC", ofst), nil
		}
	}

	return nil, ErrUnknownTimeZone.GenByArgs(s)
}

func setSnapshotTS(s *SessionVars, sVal string) error {
	if sVal == "" {
		s.SnapshotTS = 0
		return nil
	}

	if tso, err := strconv.ParseUint(sVal, 10, 64); err == nil {
		s.SnapshotTS = tso
		return nil
	}

	t, err := types.ParseTime(s.StmtCtx, sVal, mysql.TypeTimestamp, types.MaxFsp)
	if err != nil {
		return errors.Trace(err)
	}

	// TODO: Consider time_zone variable.
	t1, err := t.Time.GoTime(time.Local)
	s.SnapshotTS = GoTimeToTS(t1)
	return errors.Trace(err)
}

// GoTimeToTS converts a Go time to uint64 timestamp.
func GoTimeToTS(t time.Time) uint64 {
	ts := (t.UnixNano() / int64(time.Millisecond)) << epochShiftBits
	return uint64(ts)
}
