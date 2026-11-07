"""Writes the generated data file the tests read."""
import os

os.makedirs("build", exist_ok=True)
with open("build/data.txt", "w") as f:
    f.write("42\n")
