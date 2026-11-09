package gameserver

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"rf4server/internal/proto"
	"rf4server/internal/store"
)

func nativeInventoryContainers(kind uint64) []string {
	switch kind {
	case containerBackpack:
		return []string{"backpack", "rig"}
	case 17, 18:
		return []string{fmt.Sprintf("native:%d", kind)}
	case 10, 11:
		// 房屋仓库（容器枚举 10/11 统一落 item.container='house'，与
		// containerEnumName 一致）。缺这条映射时 4/1 计数与 4/4 列表
		// 对房屋容器一律回空，客户端房屋仓库 UI 永远无数据。
		return []string{"house"}
	}
	return nil
}

// patchNativeContainer 容器枚举 wire 字段（mfllbndghhe/csSlotEnum）以 DB
// 容器列回填发送副本：行存储不持久化该字段（启动修复 RepairItemPhysics 恒
// 清零，见 prepareNewCard 注释），但客户端谓词按它判断——右键出售要容器
// ==18 且未指派（RUITradingCardCell，2026-11-08 逆向）。仅 native:<枚举>
// 容器（17/18）需要；backpack 等命名容器官方语义未证，不动。keepnet 树
// fields[18] 回填 containerKeepnet 是同款先例。
func patchNativeContainer(row *store.Item, tree *proto.Value) {
	if row == nil || tree == nil || !strings.HasPrefix(row.Container, "native:") {
		return
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(row.Container, "native:"), 10, 8)
	if err != nil {
		return
	}
	if idx := fieldIdxByCS(row.WireClass, csSlotEnum); idx >= 0 && idx < len(tree.Fields) {
		tree.Fields[idx].Num = ptrU64(n)
	}
}

// 导入快照保留渔获资料，容器和版本以当前护网为准。
// 物种稀有度（f4）始终以静态表为准——旧快照可能缺失或为 0。
func (s *Server) keepnetItemTree(ctx context.Context, entry *store.KeepnetEntry, level string) *proto.Value {
	rarity := s.DB.SpeciesRarity(ctx, entry.Species)
	row, err := s.DB.GetItemByGUID(ctx, entry.EchoGUID)
	if err == nil && row.UID == entry.UID && row.Container == "keepnet" && row.TypeID == 25 && row.WireClass == "ecknbkpeeji" {
		if fields, err := FieldsFromJSON(row.FieldsJSON); err == nil && len(fields) == 29 {
			fields[4].Num = ptrU64(uint64(rarity))
			fields[16].Hex = entry.EchoGUID
			fields[18].Num = ptrU64(containerKeepnet)
			fields[22].Num = ptrU64(uint64(netTicksNow()))
			return proto.Obj(row.WireClass, uint32(row.TypeID), fields...)
		}
	}
	return buildKeepnet25(entry, level, rarity)
}

func inventoryContainerMatches(name string, allowed []string) bool {
	for _, value := range allowed {
		if name == value {
			return true
		}
	}
	return false
}
