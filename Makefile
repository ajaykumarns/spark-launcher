PYTHON ?= python3
.PHONY: test node-helper

test:
	PYTHONPATH=src $(PYTHON) -m unittest discover -s tests -v

node-helper:
	$(PYTHON) tools/build_node_helper.py
