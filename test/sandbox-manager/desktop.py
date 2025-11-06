import time

from e2b_desktop import Sandbox

# Create a new desktop sandbox
desktop = Sandbox.create(api_key="GG", template="desktop")
print(f"sandboxId: {desktop.sandbox_id}")
# Launch an application
desktop.launch('google-chrome')  # or vscode, firefox, etc.
print("waiting some seconds for desktop launching")
time.sleep(5)
# Stream the application's window
# Note: There can be only one stream at a time
# You need to stop the current stream before streaming another application
desktop.stream.start(
    # window_id=desktop.get_current_window_id(), # if not provided the whole desktop will be streamed
    require_auth=True
)

# Get the stream auth key
auth_key = desktop.stream.get_auth_key()

# Print the stream URL
print('Stream URL:', desktop.stream.get_url(auth_key=auth_key))

input("select the address bar and press ENTER to visit baidu.com")
desktop.write("www.baidu.com")
time.sleep(0.5)
desktop.press("enter")

input("press ENTER to exit")


# Kill the sandbox after the tasks are finished
desktop.kill()