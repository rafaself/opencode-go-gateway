# Return true only for a non-empty Codex assistant message event.
def nonempty_string:
	type == "string" and length > 0;

def assistant_item:
	(.item? | .type?) == "agent_message";

def assistant_output_event:
	assistant_item and (
		(
			(.type == "item.started" or .type == "item.updated" or .type == "item.completed")
			and (.item? | .text? | nonempty_string)
		)
		or
		(
			.type == "item.delta"
			and (.delta? | nonempty_string)
		)
	);

if type == "array" then
	any(.[]; assistant_output_event)
else
	assistant_output_event
end
