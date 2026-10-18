Known good: `page_count` rounds up (the two failing tests pass), only `shop/pagination.py` changes, the tests are untouched.
A good run takes about 6 turns: read the test, read the module, one edit, run the tests, stop. Worth noting: whether it runs the
tests itself before and after, and whether the final message says what was wrong.
