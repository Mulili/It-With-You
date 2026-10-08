package tool

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 查时间这个工具要真的给出**当前**时间，而不是一个固定的样例值。
//
// 断言方式：拿"今天"与"今年"去比——不写死具体数值，否则测试到第二天就红。
func TestClockReturnsCurrentTime(t *testing.T) {
	out, err := Clock{}.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	now := time.Now()
	if !strings.Contains(out, now.Format("2006-01-02")) {
		t.Errorf("结果里该有今天的日期（%s）：%q", now.Format("2006-01-02"), out)
	}
	if !strings.Contains(out, weekdayCN(now)) {
		t.Errorf("结果里该有中文星期（%s）：%q", weekdayCN(now), out)
	}
	// 中文星期不能退化成英文枚举（Go 的 Weekday.String() 会印出 "Thursday"）
	if strings.Contains(out, "Monday") || strings.Contains(out, "Thursday") {
		t.Errorf("星期应当是中文：%q", out)
	}
}

// Spec 是这个工具**唯一**的自我描述，模型全靠它决定调不调、怎么调。
func TestClockSpec(t *testing.T) {
	spec := Clock{}.Spec()
	if spec.Type != "function" || spec.Function.Name != "get_current_time" {
		t.Errorf("声明形状不对：%+v", spec)
	}
	// 描述里要写清"什么时候别用"——只写"查询当前时间"的话，
	// 模型会在每次闲聊开场都调一下（那是真机上会被用户看出来的毛病）
	if !strings.Contains(spec.Function.Description, "不要调用") {
		t.Errorf("描述里该有「什么时候别用」的说明：%q", spec.Function.Description)
	}
	if spec.Function.Parameters == nil {
		t.Error("参数 schema 不能是 nil：没有参数也要显式写成空对象")
	}
}

// 注册表要把"给模型看的声明"与"按名字执行"这两件事对起来。
func TestRegistrySpecsAndCall(t *testing.T) {
	r := Builtins()

	specs := r.Specs()
	if len(specs) != 1 || specs[0].Function.Name != "get_current_time" {
		t.Fatalf("内置工具集应当只有查时间，实际 %+v", specs)
	}
	if r.Empty() {
		t.Error("内置工具集不该是空的")
	}

	out, err := r.Call(context.Background(), "get_current_time", "")
	if err != nil {
		t.Fatalf("调用内置工具失败: %v", err)
	}
	if !strings.Contains(out, "现在是") {
		t.Errorf("结果该是一句能直接给模型看的话：%q", out)
	}
}

// 模型编一个不存在的工具名时，返回的是**错误**而不是 panic——调用方会把它翻成
// 一句给模型看的话，她自己就纠正了（这是 function calling 的常规姿态）。
func TestRegistryCallUnknownTool(t *testing.T) {
	r := Builtins()

	if _, err := r.Call(context.Background(), "no_such_tool", "{}"); err == nil {
		t.Fatal("未知工具应当报错")
	} else if !strings.Contains(err.Error(), "get_current_time") {
		t.Errorf("错误里该列出可用的工具名（模型据此纠正）：%v", err)
	}
}

// 空注册表是合法状态：它代表"这一轮不带工具"，于是请求里连 tools 字段都不出现。
func TestEmptyRegistry(t *testing.T) {
	r := NewRegistry()

	if !r.Empty() {
		t.Error("没有工具时 Empty() 应当为 true")
	}
	if specs := r.Specs(); specs != nil {
		t.Errorf("没有工具时不该给出声明（否则请求会带一个空的 tools 数组）：%+v", specs)
	}
	// nil 注册表也要安全：App 里它有可能是 nil
	var nilReg *Registry
	if !nilReg.Empty() {
		t.Error("nil 注册表也应当被当成空的")
	}
	if _, err := nilReg.Call(context.Background(), "x", "{}"); err == nil {
		t.Error("nil 注册表调用应当报错而不是 panic")
	}
}
