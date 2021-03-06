// Copyright (C) 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vulkan

import (
	"context"
	"fmt"

	"github.com/google/gapid/core/image"
	"github.com/google/gapid/core/log"
	"github.com/google/gapid/gapis/api"
	"github.com/google/gapid/gapis/api/sync"
	"github.com/google/gapid/gapis/api/transform"
	"github.com/google/gapid/gapis/capture"
	"github.com/google/gapid/gapis/resolve"
	"github.com/google/gapid/gapis/resolve/dependencygraph"
	"github.com/google/gapid/gapis/service/path"
)

type CustomState struct {
	SubCmdIdx         api.SubCmdIdx
	CurrentSubmission *api.Cmd
	PreSubcommand     func(interface{})
	PostSubcommand    func(interface{})
	AddCommand        func(interface{})
	IsRebuilding      bool
	pushMarkerGroup   func(name string, next bool, ty MarkerType)
	popMarkerGroup    func(ty MarkerType)
}

func getStateObject(s *api.State) *State {
	return GetState(s)
}

type VulkanContext struct{}

// Name returns the display-name of the context.
func (VulkanContext) Name() string {
	return "Vulkan Context"
}

// ID returns the context's unique identifier.
func (VulkanContext) ID() api.ContextID {
	// ID returns the context's unique identifier
	return api.ContextID{1}
}

// API returns the vulkan API.
func (VulkanContext) API() api.API {
	return API{}
}

func (API) Context(s *api.State, thread uint64) api.Context {
	return VulkanContext{}
}

func (c *State) preMutate(ctx context.Context, s *api.State, cmd api.Cmd) error {
	return nil
}

func (API) GetFramebufferAttachmentInfo(state *api.State, thread uint64, attachment api.FramebufferAttachment) (w, h uint32, a uint32, f *image.Format, err error) {
	w, h, form, i, err := GetState(state).getFramebufferAttachmentInfo(attachment)
	switch attachment {
	case api.FramebufferAttachment_Stencil:
		return 0, 0, 0, nil, fmt.Errorf("Unsupported Stencil")
	case api.FramebufferAttachment_Depth:
		format, err := getDepthImageFormatFromVulkanFormat(form)
		if err != nil {
			return 0, 0, 0, nil, fmt.Errorf("Unknown format for Depth attachment")
		}
		return w, h, i, format, err
	default:
		format, err := getImageFormatFromVulkanFormat(form)
		if err != nil {
			return 0, 0, 0, nil, fmt.Errorf("Unknown format for Color attachment")
		}
		return w, h, i, format, err
	}
}

// Mesh implements the api.MeshProvider interface
func (API) Mesh(ctx context.Context, o interface{}, p *path.Mesh) (*api.Mesh, error) {
	switch dc := o.(type) {
	case *VkQueueSubmit:
		return drawCallMesh(ctx, dc, p)
	}
	return nil, fmt.Errorf("Cannot get the mesh data from %v", o)
}

type MarkerType int

const (
	DebugMarker = iota
	RenderPassMarker
)

type markerInfo struct {
	name   string
	ty     MarkerType
	start  uint64
	end    uint64
	parent api.SubCmdIdx
}

func (API) ResolveSynchronization(ctx context.Context, d *sync.Data, c *path.Capture) error {
	ctx = capture.Put(ctx, c)
	st, err := capture.NewState(ctx)
	if err != nil {
		return err
	}
	cmds, err := resolve.Cmds(ctx, c)
	if err != nil {
		return err
	}
	s := GetState(st)

	i := api.CmdID(0)
	submissionMap := make(map[*api.Cmd]api.CmdID)
	commandMap := make(map[*api.Cmd]api.CmdID)
	lastSubcommand := api.SubCmdIdx{}
	lastCmdIndex := api.CmdID(0)

	// Prepare for collect marker groups
	// Stacks of open markers for each VkQueue
	markerStack := map[VkQueue][]*markerInfo{}
	// Stacks of markers to be opened in the next subcommand for each VkQueue
	markersToOpen := map[VkQueue][]*markerInfo{}
	s.pushMarkerGroup = func(name string, next bool, ty MarkerType) {
		vkQu := (*s.CurrentSubmission).(*VkQueueSubmit).Queue
		if next {
			// Add to the to-open marker stack, marker will be opened in the next
			// subcommand
			stack := markersToOpen[vkQu]
			markersToOpen[vkQu] = append(stack, &markerInfo{name: name, ty: ty})
		} else {
			// Add to the marker stack
			stack := markerStack[vkQu]
			fullCmdIdx := api.SubCmdIdx{uint64(submissionMap[s.CurrentSubmission])}
			fullCmdIdx = append(fullCmdIdx, s.SubCmdIdx...)
			marker := &markerInfo{name: name,
				ty:     ty,
				start:  fullCmdIdx[len(fullCmdIdx)-1],
				end:    uint64(0),
				parent: fullCmdIdx[0 : len(fullCmdIdx)-1]}
			markerStack[vkQu] = append(stack, marker)
		}
	}
	s.popMarkerGroup = func(ty MarkerType) {
		vkQu := (*s.CurrentSubmission).(*VkQueueSubmit).Queue
		stack := markerStack[vkQu]
		if len(stack) == 0 {
			log.W(ctx, "Cannot pop marker with type: %v, no open marker with same type at: VkQueueSubmit ID: %v, SubCmdIdx: %v",
				ty, submissionMap[s.CurrentSubmission], s.SubCmdIdx)
			return
		}
		// If the type of the top marker in the stack does not match with the
		// request type, pop until a matching marker is found and pop it. The
		// spilled markers are processed in the following way: if it is a debug
		// marker, resurrect it in the next subcommand, if it is a renderpass
		// marker, discard it.
		top := len(stack) - 1
		for top >= 0 && stack[top].ty != ty {
			log.W(ctx, "Type of the top marker does not match with the pop request")
			end := s.SubCmdIdx[len(s.SubCmdIdx)-1] + 1
			d.SubCommandMarkerGroups.NewMarkerGroup(stack[top].parent, stack[top].name, stack[top].start, end)
			switch stack[top].ty {
			case DebugMarker:
				markersToOpen[vkQu] = append(markersToOpen[vkQu], stack[top])
				log.D(ctx, "Debug marker popped due to popping renderpass marker, new debug marker group will be opened again in the next subcommand")
			default:
				log.W(ctx, "Renderpass marker popped due to popping debug marker, renderpass marker group will be closed here")
			}
			top--
		}
		// Update the End value of the debug marker and create new group.
		if top >= 0 {
			end := s.SubCmdIdx[len(s.SubCmdIdx)-1] + 1
			d.SubCommandMarkerGroups.NewMarkerGroup(stack[top].parent, stack[top].name, stack[top].start, end)
			markerStack[vkQu] = stack[0:top]
		} else {
			markerStack[vkQu] = []*markerInfo{}
		}
	}

	s.PreSubcommand = func(interface{}) {
		// Update the submission map before execute subcommand callback and
		// postSubCommand callback.
		if _, ok := submissionMap[s.CurrentSubmission]; !ok {
			submissionMap[s.CurrentSubmission] = i
		}
		// Examine the marker stack. If the comming subcommand is submitted in a
		// different command buffer or submission batch or VkQueueSubmit call, and
		// there are unclosed marker group, we need to 1) check whether the
		// unclosed marker groups are opened in secondary command buffers, log
		// error and pop them.  2) Close all the unclosed "debug marker" group, and
		// begin new groups for the new command buffer. Note that only "debug
		// marker" groups are resurrected in this step, all unclosed "renderpass
		// markers" are assumed closed.
		// Finally, no matter whether the comming subcommand is in a different
		// command buffer or submission batch, If there are pending markers in the
		// to-open stack, begin new groups for those pending markers.
		vkQu := (*s.CurrentSubmission).(*VkQueueSubmit).Queue
		stack := markerStack[vkQu]
		fullCmdIdx := api.SubCmdIdx{uint64(submissionMap[s.CurrentSubmission])}
		fullCmdIdx = append(fullCmdIdx, s.SubCmdIdx...)

		for lastCmdIndex != api.CmdID(0) && len(stack) > 0 {
			top := stack[len(stack)-1]
			if len(top.parent) > len(fullCmdIdx) {
				// The top of the stack is an unclosed debug marker group which is
				// opened in a secondary command buffer. This debug marker group will
				// be closed here, the End value of the group will be the last updated
				// value (which should be one plus the last command index in its
				// secondary command buffer).
				log.E(ctx, "DebugMarker began in secondary command buffer does not close. Close now")
				d.SubCommandMarkerGroups.NewMarkerGroup(top.parent, top.name, top.start, top.end)
				stack = stack[0 : len(stack)-1]
				continue
			}
			break
		}
		// Close all the unclosed debug marker groups that are opened in previous
		// submissions or command buffers. Those closed groups will have their
		// End value to be the last updated value, and new groups with same name
		// will be opened in the new command buffer.
		if lastCmdIndex != api.CmdID(0) && len(stack) > 0 &&
			!stack[len(stack)-1].parent.Contains(fullCmdIdx) {
			originalStack := []*markerInfo(stack)
			markerStack[vkQu] = []*markerInfo{}
			for _, o := range originalStack {
				s.pushMarkerGroup(o.name, false, DebugMarker)
			}
		}
		// Open new groups for the pending markers in the to-open stack
		toOpenStack := markersToOpen[vkQu]
		i := len(toOpenStack) - 1
		for i >= 0 {
			s.pushMarkerGroup(toOpenStack[i].name, false, toOpenStack[i].ty)
			i--
		}
		markersToOpen[vkQu] = []*markerInfo{}
	}

	s.PostSubcommand = func(a interface{}) {
		// We do not record/handle any subcommands inside any of our
		// rebuild commands
		if s.IsRebuilding {
			return
		}

		data := a.(CommandBufferCommand)
		rootIdx := api.CmdID(i)
		if k, ok := submissionMap[s.CurrentSubmission]; ok {
			rootIdx = api.CmdID(k)
		} else {
			submissionMap[s.CurrentSubmission] = i
		}
		// No way for this to not exist, we put it in up there
		k := submissionMap[s.CurrentSubmission]
		if v, ok := d.SubcommandReferences[k]; ok {
			v = append(v,
				sync.SubcommandReference{append(api.SubCmdIdx(nil), s.SubCmdIdx...), commandMap[data.initialCall], false})
			d.SubcommandReferences[k] = v
		} else {
			d.SubcommandReferences[k] = []sync.SubcommandReference{
				sync.SubcommandReference{append(api.SubCmdIdx(nil), s.SubCmdIdx...), commandMap[data.initialCall], false}}
		}

		previousIndex := append(api.SubCmdIdx(nil), s.SubCmdIdx...)
		previousIndex.Decrement()
		if !previousIndex.Equals(lastSubcommand) && lastCmdIndex != api.CmdID(0) {
			if v, ok := d.SubcommandGroups[lastCmdIndex]; ok {
				v = append(v, append(api.SubCmdIdx(nil), lastSubcommand...))
				d.SubcommandGroups[lastCmdIndex] = v
			} else {
				d.SubcommandGroups[lastCmdIndex] = []api.SubCmdIdx{append(api.SubCmdIdx(nil), lastSubcommand...)}
			}
			lastSubcommand = append(api.SubCmdIdx(nil), s.SubCmdIdx...)
			lastCmdIndex = k
		} else {
			lastSubcommand = append(api.SubCmdIdx(nil), s.SubCmdIdx...)
			lastCmdIndex = k
		}

		if rng, ok := d.CommandRanges[rootIdx]; ok {
			rng.LastIndex = append(api.SubCmdIdx(nil), s.SubCmdIdx...)
			rng.Ranges[i] = rng.LastIndex
			d.CommandRanges[rootIdx] = rng
		} else {
			er := sync.ExecutionRanges{
				LastIndex: append(api.SubCmdIdx(nil), s.SubCmdIdx...),
				Ranges:    make(map[api.CmdID]api.SubCmdIdx),
			}
			er.Ranges[i] = append(api.SubCmdIdx(nil), s.SubCmdIdx...)
			d.CommandRanges[rootIdx] = er
		}

		// Update the End value for all unclosed debug marker groups
		vkQu := (*s.CurrentSubmission).(*VkQueueSubmit).Queue
		for _, ms := range markerStack[vkQu] {
			// If the last subcommand is in a secondary command buffer and current
			// recording debug marker groups are opened in a primary command buffer,
			// this will assign a wrong End value to the open marker groups.
			// However, those End values will be overwritten when the secondary
			// command buffer ends and vkCmdExecuteCommands get executed.
			ms.end = s.SubCmdIdx[len(s.SubCmdIdx)-1] + 1
		}
	}

	s.AddCommand = func(a interface{}) {
		data := a.(CommandBufferCommand)
		commandMap[data.initialCall] = i
	}

	err = api.ForeachCmd(ctx, cmds, func(ctx context.Context, id api.CmdID, cmd api.Cmd) error {
		i = id
		cmd.Mutate(ctx, id, st, nil)
		return nil
	})
	if err != nil {
		return err
	}

	if lastCmdIndex != api.CmdID(0) {
		if v, ok := d.SubcommandGroups[lastCmdIndex]; ok {
			v = append(v, append(api.SubCmdIdx(nil), lastSubcommand...))
			d.SubcommandGroups[lastCmdIndex] = v
		} else {
			d.SubcommandGroups[lastCmdIndex] = []api.SubCmdIdx{
				append(api.SubCmdIdx(nil), lastSubcommand...)}
		}
	}
	return nil
}

// Interface check
var _ sync.SynchronizedAPI = &API{}

func (API) GetTerminator(ctx context.Context, c *path.Capture) (transform.Terminator, error) {
	return NewVulkanTerminator(ctx, c)
}

func (API) MutateSubcommands(ctx context.Context, id api.CmdID, cmd api.Cmd,
	s *api.State, preSubCmdCb func(*api.State, api.SubCmdIdx, api.Cmd),
	postSubCmdCb func(*api.State, api.SubCmdIdx, api.Cmd)) error {
	c := GetState(s)
	if postSubCmdCb != nil {
		c.PostSubcommand = func(interface{}) {
			postSubCmdCb(s, append(api.SubCmdIdx{uint64(id)}, c.SubCmdIdx...), cmd)
		}
	}
	if preSubCmdCb != nil {
		c.PreSubcommand = func(interface{}) {
			preSubCmdCb(s, append(api.SubCmdIdx{uint64(id)}, c.SubCmdIdx...), cmd)
		}
	}
	if err := cmd.Mutate(ctx, id, s, nil); err != nil && err == context.Canceled {
		return err
	}
	return nil
}

// FootprintBuilder implements dependencygraph.FootprintBuilderProvider interface
func (API) FootprintBuilder(ctx context.Context) dependencygraph.FootprintBuilder {
	return newFootprintBuilder()
}
