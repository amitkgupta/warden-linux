default: all

# Proxy any target to the Makefiles in the per-tool directories
%:
	cd wsh && $(MAKE) $@
	cd oom && $(MAKE) $@
	cd repquota && $(MAKE) $@

.PHONY: default
