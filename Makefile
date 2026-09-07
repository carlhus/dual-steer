# SPDX-License-Identifier: GPL-2.0
# The external kernel tree is read-only; no build artifacts are written there.
KERNEL_SRC ?= /home/ubuntu/atsss/ATSSS-UE/mptcp_net-next
CLANG ?= clang
CC ?= cc
PYTHON ?= python3
ARCH_INCLUDE ?= /usr/include/$(shell $(CC) -dumpmachine)
BUILD ?= build
BPF_INCLUDES = -Iinclude -I$(KERNEL_SRC)/tools/testing/selftests/bpf \
	-I$(KERNEL_SRC)/tools/lib -I$(BUILD)/include/bpf -I$(ARCH_INCLUDE)
CFLAGS ?= -O2 -g -Wall -Wextra -Werror

.PHONY: all test test-c test-go agent bpf clean check-kernel-patch qemu-kernel qemu-initramfs qemu-test kernel-map-test
all: test bpf

$(BUILD):
	mkdir -p $@

$(BUILD)/select_test: tests/select_test.c include/dualsteer_policy.h include/dualsteer_select.h | $(BUILD)
	$(CC) $(CFLAGS) -std=c11 -Iinclude $< -o $@

test: test-c test-go

test-c: $(BUILD)/select_test
	./$(BUILD)/select_test

test-go:
	cd dualsteer-agent && go test ./... && go vet ./...

agent: | $(BUILD)
	cd dualsteer-agent && go build -o $(abspath $(BUILD))/dualsteer-agent ./cmd/dualsteer-agent

$(BUILD)/include/bpf/bpf_helper_defs.h:
	mkdir -p $(dir $@)
	$(PYTHON) $(KERNEL_SRC)/scripts/bpf_doc.py --header \
		--filename $(KERNEL_SRC)/include/uapi/linux/bpf.h > $@

$(BUILD)/dualsteer.bpf.o: bpf/dualsteer.bpf.c include/dualsteer_policy.h include/dualsteer_select.h $(BUILD)/include/bpf/bpf_helper_defs.h
	$(CLANG) -target bpf -D__TARGET_ARCH_x86 -D__bitwise= -O2 -g \
		-Wall -Werror $(BPF_INCLUDES) -c $< -o $@

bpf: $(BUILD)/dualsteer.bpf.o

check-kernel-patch:
	git -C $(KERNEL_SRC) apply --check $(CURDIR)/bpf/mptcp-default-kfunc.patch

kernel-map-test: | $(BUILD)
	cd dualsteer-agent && go test -c -o $(abspath $(BUILD))/agent-kernel-tests
	sudo -n env DUALSTEER_KERNEL_TEST=1 $(BUILD)/agent-kernel-tests -test.run '^TestKernelMaps$$' -test.v

qemu-kernel:
	bash scripts/qemu-kernel.sh

qemu-initramfs:
	bash scripts/qemu-initramfs.sh

qemu-test: agent bpf qemu-initramfs
	bash scripts/qemu-test.sh

clean:
	rm -rf $(BUILD)
