---
trigger: model_decision
description: When writing unit tests, this rule must be followed
---

1. Before writing unit tests, you should thoroughly review the code under test to ensure you understand its
   functionality and implementation. If you find errors in the code being tested, you should stop testing and provide
   the user with a review report and suggestions for improvement, but do not modify the code directly.
2. When performing unit test writing tasks, modifying code files is prohibited.
3. When attempting to run and debug unit tests, if three attempts fail to pass, you should immediately stop trying to
   modify and request assistance from the user.
4. If during testing you believe the code under test has errors, you should immediately stop making modifications and
   request assistance from the user.
