// Package timeutil 是跨包共用的时间工具。
//
// 为什么单独成包：项目里所有时间戳统一用 Unix 毫秒，而"取当前时间"这件事原来在
// history 与 memory 两个平级包里各写了一份——两份注释都自称是"统一入口"，
// 但两个平级包互相不依赖，谁也统一不了谁。这正是"看起来统一、其实各写各的"的典型，
// 所以上移到这里，由两边共同依赖。
package timeutil

import "time"

// NowMillis 取当前时间（Unix 毫秒）。项目内所有时间戳都从这里取。
func NowMillis() int64 { return time.Now().UnixMilli() }
